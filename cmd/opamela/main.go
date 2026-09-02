// Command opamela serves a mirror of opam-repository whose packages point back
// at it, so that building OCaml code reaches exactly one host.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jeremiegoldberg/opamela/internal/fetch"
	"github.com/jeremiegoldberg/opamela/internal/gitrepo"
	"github.com/jeremiegoldberg/opamela/internal/mirror"
	"github.com/jeremiegoldberg/opamela/internal/opamrepo"
	"github.com/jeremiegoldberg/opamela/internal/server"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const defaultUpstream = "https://github.com/ocaml/opam-repository"

type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatedFlag) Set(v string) error {
	if v == "" {
		return errors.New("empty value")
	}
	*f = append(*f, v)
	return nil
}

func main() {
	var overlays repeatedFlag

	var (
		listen       = flag.String("listen", ":8080", "address to listen on")
		baseURL      = flag.String("base-url", "", "public root URL of this server, as builds will see it (required)")
		stateDir     = flag.String("state", "/var/lib/opamela", "directory holding the checkouts, the generated repository and the archive cache")
		upstream     = flag.String("upstream", defaultUpstream, "opam-repository to mirror")
		refresh      = flag.Duration("refresh", time.Hour, "how often to look for new packages upstream; 0 disables refreshing")
		workers      = flag.Int("workers", 0, "parallel package rewrites (0 = one per CPU)")
		fetchTimeout = flag.Duration("fetch-timeout", 10*time.Minute, "time limit for downloading one archive")
		buildOnly    = flag.Bool("build-only", false, "build the mirror once, report, and exit without serving")
		showVersion  = flag.Bool("version", false, "print the version and exit")
	)
	flag.Var(&overlays, "overlay", "additional repository whose packages take precedence; repeatable")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"opamela %s - an opam-repository mirror\n\nUsage:\n  opamela -base-url https://opam.internal [flags]\n\nFlags:\n",
			version)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("opamela", version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if *baseURL == "" {
		log.Error("-base-url is required: rewritten packages have to name a host builds can reach")
		os.Exit(2)
	}
	if err := run(log, config{
		listen:       *listen,
		baseURL:      strings.TrimSuffix(*baseURL, "/"),
		stateDir:     *stateDir,
		upstream:     *upstream,
		overlays:     overlays,
		refresh:      *refresh,
		workers:      *workers,
		fetchTimeout: *fetchTimeout,
		buildOnly:    *buildOnly,
	}); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type config struct {
	listen       string
	baseURL      string
	stateDir     string
	upstream     string
	overlays     []string
	refresh      time.Duration
	workers      int
	fetchTimeout time.Duration
	buildOnly    bool
}

func run(log *slog.Logger, cfg config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := &manager{
		log:     log,
		cfg:     cfg,
		builder: &mirror.Builder{BaseURL: cfg.baseURL, Workers: cfg.workers},
		base:    &gitrepo.Repo{URL: cfg.upstream, Dir: filepath.Join(cfg.stateDir, "upstream", "base")},
	}
	for i, url := range cfg.overlays {
		m.overlays = append(m.overlays, &gitrepo.Repo{
			URL: url,
			Dir: filepath.Join(cfg.stateDir, "upstream", fmt.Sprintf("overlay-%d", i)),
		})
	}

	if cfg.buildOnly {
		st, err := m.rebuild(ctx)
		if err != nil {
			return err
		}
		log.Info("build complete", "rev", st.Rev, "mirror", st.MirrorDir)
		return nil
	}

	cache := fetch.New(ctx, filepath.Join(cfg.stateDir, "archives"), &http.Client{
		Timeout: 0, // bounded per download by the cache
	}, cfg.fetchTimeout, log)

	srv := server.New(cache, log)
	httpSrv := &http.Server{
		Addr:              cfg.listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	// Listen before the first build so that health checks and orchestrators
	// get an honest "still building" rather than a refused connection.
	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", ln.Addr().String(), "base-url", cfg.baseURL, "version", version)

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	go m.run(ctx, srv)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// manager owns the checkouts and the generated trees.
type manager struct {
	log      *slog.Logger
	cfg      config
	builder  *mirror.Builder
	base     *gitrepo.Repo
	overlays []*gitrepo.Repo

	rev  string // revision currently published
	prev string // previous generated tree, removed on the next successful build
}

// run performs the first build, then refreshes on a timer.
//
// The prototype this replaces built its index once, at startup. A package
// published upstream simply did not exist for the mirror until somebody
// restarted it, and since restarting fixed it, nobody ever wrote this loop.
func (m *manager) run(ctx context.Context, srv *server.Server) {
	if st, err := m.rebuild(ctx); err != nil {
		m.log.Error("initial build failed", "err", err)
	} else {
		srv.SetState(st)
	}

	if m.cfg.refresh <= 0 {
		m.log.Info("refreshing disabled")
		return
	}

	t := time.NewTicker(m.cfg.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st, err := m.rebuild(ctx)
			switch {
			case errors.Is(err, errUnchanged):
				m.log.Debug("upstream unchanged", "rev", m.rev)
			case err != nil:
				m.log.Error("refresh failed, keeping the current mirror", "err", err)
			default:
				srv.SetState(st)
			}
		}
	}
}

var errUnchanged = errors.New("upstream unchanged")

// rebuild syncs the checkouts and regenerates the mirror if anything moved.
//
// A failed refresh is not an outage: the published tree is only replaced once a
// new one has been generated in full. A mirror that empties itself because
// GitHub was briefly unreachable would be worse than no mirror at all.
func (m *manager) rebuild(ctx context.Context) (server.State, error) {
	revs := make([]string, 0, len(m.overlays)+1)

	baseRev, err := m.base.Sync(ctx)
	if err != nil {
		return server.State{}, err
	}
	revs = append(revs, baseRev)

	sources := opamrepo.Sources{Base: m.base.Dir}
	for _, o := range m.overlays {
		rev, err := o.Sync(ctx)
		if err != nil {
			return server.State{}, err
		}
		revs = append(revs, rev)
		sources.Overlays = append(sources.Overlays, o.Dir)
	}

	rev := combinedRev(revs)
	if rev == m.rev {
		return server.State{}, errUnchanged
	}

	staging := filepath.Join(m.cfg.stateDir, "mirror-"+rev)
	m.log.Info("building mirror", "rev", rev, "dir", staging)

	start := time.Now()
	stats, err := m.builder.Build(ctx, sources, staging)
	if err != nil {
		os.RemoveAll(staging)
		return server.State{}, err
	}
	m.log.Info("mirror built", "rev", rev, "took", time.Since(start).Round(time.Millisecond),
		"packages", stats.Packages, "rewritten", stats.Rewritten,
		"sourceless", stats.Sourceless, "passthrough", stats.Passthrough)

	// At most two generated trees exist at once: the one being served and the
	// one it replaced, kept until the next build so that requests still
	// reading the old tree are never pulled out from under.
	if m.prev != "" && m.prev != staging {
		if err := os.RemoveAll(m.prev); err != nil {
			m.log.Warn("removing previous mirror", "dir", m.prev, "err", err)
		}
	}
	m.prev, m.rev = staging, rev

	return server.State{
		MirrorDir: staging,
		Pristine:  sources,
		Rev:       rev,
		BuiltAt:   time.Now(),
	}, nil
}

func combinedRev(revs []string) string {
	short := make([]string, 0, len(revs))
	for _, r := range revs {
		if len(r) > 12 {
			r = r[:12]
		}
		short = append(short, r)
	}
	return strings.Join(short, "+")
}
