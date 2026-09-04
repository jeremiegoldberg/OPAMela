(* Command opamela: serve a mirror of opam-repository whose packages point back
   at it, so that building OCaml code reaches exactly one host. OCaml port of the
   Go command. *)

open Opamela

let version = "dev"

let default_upstream = "https://github.com/ocaml/opam-repository"

let print_help () =
  Printf.printf
    {|opamela %s - a mirror of opam-repository.

It rewrites the repository index so a whole CI fleet fetches opam packages from
one host on its own network, instead of re-downloading them from the public
internet on every build. It works with unmodified opam.

Usage:
  opamela -base-url <url> [flags]
  opamela help
  opamela -version

Examples:
  # Serve a mirror on :8080, cloning ocaml/opam-repository under -state.
  opamela -base-url https://opam.internal

  # Also merge the dune-universe overlay (needed for opam-monorepo).
  opamela -base-url https://opam.internal \
      -overlay https://github.com/dune-universe/opam-overlays

  # Build the mirror once and exit, without serving.
  opamela -base-url https://opam.internal -build-only

Point opam at it once it is up:
  opam repository set-url default https://opam.internal && opam update

Flags:
  -base-url URL     public root URL of this server, as builds will see it (required)
  -listen ADDR      address to listen on (default ":8080")
  -state DIR        checkouts, generated repository and archive cache (default "/var/lib/opamela")
  -upstream URL     opam-repository to mirror (default %s)
  -overlay URL      extra repository whose packages take precedence; repeatable
  -refresh DUR      how often to look for new packages, e.g. 1h, 30m, 0 disables (default 1h)
  -fetch-timeout DUR  time limit for downloading one archive (default 10m)
  -build-only       build the mirror once, report, and exit without serving
  -version          print the version and exit
  -help             print this help and exit

Exit codes:
  0  success
  1  a runtime error (build failed, cannot listen, upstream unreachable)
  2  a usage error (missing -base-url, unknown flag or argument)

Documentation: https://github.com/jeremiegoldberg/OPAMela
|}
    version default_upstream

(* accept 1h / 30m / 3600s / 3600, returning seconds *)
let parse_duration s : float =
  let n = String.length s in
  if n = 0 then 0.
  else
    let num, mult =
      match s.[n - 1] with
      | 's' -> (String.sub s 0 (n - 1), 1.)
      | 'm' -> (String.sub s 0 (n - 1), 60.)
      | 'h' -> (String.sub s 0 (n - 1), 3600.)
      | '0' .. '9' -> (s, 1.)
      | c -> failwith (Printf.sprintf "invalid duration %S (bad suffix %c)" s c)
    in
    float_of_string num *. mult

let now_rfc3339 () =
  let tm = Unix.gmtime (Unix.gettimeofday ()) in
  Printf.sprintf "%04d-%02d-%02dT%02d:%02d:%02dZ" (tm.Unix.tm_year + 1900)
    (tm.Unix.tm_mon + 1) tm.Unix.tm_mday tm.Unix.tm_hour tm.Unix.tm_min tm.Unix.tm_sec

let short r = if String.length r > 12 then String.sub r 0 12 else r

let log msg = Printf.eprintf "%s %s\n%!" (now_rfc3339 ()) msg

(* manager: owns the checkouts and rebuilds the mirror *)
type manager = {
  base_url : string;
  state_dir : string;
  base : Gitrepo.t;
  overlays : Gitrepo.t list;
  mutable rev : string;
  mutable prev : string; (* previous generated tree, removed after the next build *)
}

exception Unchanged

let rebuild m : (Server.state, string) result =
  let sync_all () =
    match Gitrepo.sync m.base with
    | Error e -> Error e
    | Ok base_rev ->
        let rec go revs dirs = function
          | [] -> Ok (List.rev revs, List.rev dirs)
          | (o : Gitrepo.t) :: rest -> (
              match Gitrepo.sync o with
              | Error e -> Error e
              | Ok r -> go (r :: revs) (o.dir :: dirs) rest)
        in
        go [ base_rev ] [] m.overlays
  in
  match sync_all () with
  | Error e -> Error e
  | Ok (revs, overlay_dirs) ->
      let rev = String.concat "+" (List.map short revs) in
      if rev = m.rev then raise Unchanged;
      let staging = Filename.concat m.state_dir ("mirror-" ^ rev) in
      log (Printf.sprintf "building mirror rev=%s dir=%s" rev staging);
      let t0 = Unix.gettimeofday () in
      let src = { Opamrepo.base = m.base.dir; overlays = overlay_dirs } in
      (match Mirror.build ~base_url:m.base_url src staging with
      | Error e ->
          Mirror.rm_rf staging;
          Error e
      | Ok stats ->
          log
            (Printf.sprintf "mirror built rev=%s took=%.1fs %s" rev
               (Unix.gettimeofday () -. t0) (Mirror.stats_to_string stats));
          if m.prev <> "" && m.prev <> staging then Mirror.rm_rf m.prev;
          m.prev <- staging;
          m.rev <- rev;
          Ok
            {
              Server.mirror_dir = staging;
              pristine = src;
              rev;
              built_at = now_rfc3339 ();
            })

let () =
  let listen = ref ":8080" in
  let base_url = ref "" in
  let state_dir = ref "/var/lib/opamela" in
  let upstream = ref default_upstream in
  let overlays = ref [] in
  let refresh = ref "1h" in
  let fetch_timeout = ref "10m" in
  let build_only = ref false in
  let show_version = ref false in
  let bad_usage msg =
    Printf.eprintf "opamela: %s\n\n" msg;
    print_help ();
    exit 2
  in
  let spec =
    [
      ("-listen", Arg.Set_string listen, "");
      ("-base-url", Arg.Set_string base_url, "");
      ("-state", Arg.Set_string state_dir, "");
      ("-upstream", Arg.Set_string upstream, "");
      ("-overlay", Arg.String (fun s -> overlays := s :: !overlays), "");
      ("-refresh", Arg.Set_string refresh, "");
      ("-fetch-timeout", Arg.Set_string fetch_timeout, "");
      ("-build-only", Arg.Set build_only, "");
      ("-version", Arg.Set show_version, "");
      ("-help", Arg.Unit (fun () -> print_help (); exit 0), "");
      ("--help", Arg.Unit (fun () -> print_help (); exit 0), "");
    ]
  in
  let anon a =
    if a = "help" then (print_help (); exit 0)
    else bad_usage (Printf.sprintf "unexpected argument %S" a)
  in
  (try Arg.parse_argv Sys.argv spec anon "opamela"
   with
  | Arg.Bad msg ->
      Printf.eprintf "%s\n" (String.trim msg);
      exit 2
  | Arg.Help _ -> print_help (); exit 0);

  if !show_version then (print_endline ("opamela " ^ version); exit 0);

  if !base_url = "" then
    bad_usage "-base-url is required, since rewritten packages have to name a host your builds can reach";

  ignore !fetch_timeout;
  (* curl bounds each download; kept for CLI compatibility *)
  let overlays = List.rev !overlays in
  let base_url =
    let s = !base_url in
    if String.length s > 0 && s.[String.length s - 1] = '/' then
      String.sub s 0 (String.length s - 1)
    else s
  in

  let m =
    {
      base_url;
      state_dir = !state_dir;
      base = { Gitrepo.url = !upstream; dir = Filename.concat (Filename.concat !state_dir "upstream") "base" };
      overlays =
        List.mapi
          (fun i url ->
            { Gitrepo.url; dir = Filename.concat (Filename.concat !state_dir "upstream") (Printf.sprintf "overlay-%d" i) })
          overlays;
      rev = "";
      prev = "";
    }
  in

  if !build_only then (
    match (try rebuild m with Unchanged -> Error "unchanged") with
    | Ok st -> log (Printf.sprintf "build complete rev=%s mirror=%s" st.rev st.mirror_dir)
    | Error e -> log ("fatal: " ^ e); exit 1)
  else begin
    let cache = Fetch.create ~log (Filename.concat !state_dir "archives") in
    let srv = Server.create ~log cache in

    (* listen before the first build so health checks get an honest 503 *)
    let sock =
      try Server.bind (Server.parse_addr !listen)
      with e -> log ("fatal: cannot listen: " ^ Printexc.to_string e); exit 1
    in
    log (Printf.sprintf "listening addr=:%d base-url=%s version=%s" (Server.bound_port sock) base_url version);

    Sys.set_signal Sys.sigint (Sys.Signal_handle (fun _ -> log "shutting down"; exit 0));
    (try Sys.set_signal Sys.sigterm (Sys.Signal_handle (fun _ -> log "shutting down"; exit 0))
     with _ -> ());

    ignore (Thread.create (fun () -> Server.serve srv sock) ());

    (* initial build, then refresh on a timer *)
    (match (try rebuild m with Unchanged -> Error "unchanged") with
    | Ok st -> Server.set_state srv st
    | Error e -> log ("initial build failed: " ^ e));

    let refresh = parse_duration !refresh in
    if refresh <= 0. then (log "refreshing disabled"; while true do Unix.sleep 3600 done)
    else
      while true do
        Unix.sleepf refresh;
        match (try rebuild m with Unchanged -> Error "__unchanged__") with
        | Ok st -> Server.set_state srv st
        | Error "__unchanged__" -> ()
        | Error e -> log ("refresh failed, keeping the current mirror: " ^ e)
      done
  end
