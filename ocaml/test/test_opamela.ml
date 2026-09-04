open Opamela

let failures = ref 0
let check name cond = if not cond then (incr failures; Printf.printf "FAIL %s\n" name)
let checkeq name got want =
  if got <> want then (incr failures; Printf.printf "FAIL %s: got %S want %S\n" name got want)

let wrapped =
  "opam-version: \"2.0\"\n\
   maintainer: \"Danny Willems <contact@danny-willems.be>\"\n\
   synopsis: \"binding\"\n\
   url {\n\
  \  src:\n\
  \    \"https://github.com/dannywillems/ocaml-cordova-plugin-fcm/archive/v1.0.zip\"\n\
  \  checksum: \"md5=f43612d7e05496ff5863c40b5e8638df\"\n\
   }\n"

let checksum_list =
  "opam-version: \"2.0\"\n\
   url {\n\
  \  src: \"https://github.com/savonet/ocaml-posix/archive/v2.0.0.tar.gz\"\n\
  \  checksum: [\n\
  \    \"md5=2c186aa5161b72208a870d5710fb6208\"\n\
  \    \"sha512=d583c3d386865eab7575fc4f1976c17294bad2ee5037327cb5c3075965788170e652b7b9b9f660ef25f71558553fbcc47734b971e3c9f41627cc573d75d2fb54\"\n\
  \  ]\n\
   }\n"

let () =
  (* wrapped src *)
  let f = Opamfile.parse wrapped in
  (match f.url with
   | Some u ->
     checkeq "wrapped.src" u.src "https://github.com/dannywillems/ocaml-cordova-plugin-fcm/archive/v1.0.zip";
     check "wrapped.sum" (List.length u.checksums = 1 && (List.hd u.checksums).kind = "md5")
   | None -> check "wrapped.url" false);

  (* checksum list *)
  let f = Opamfile.parse checksum_list in
  (match f.url with
   | Some u -> check "list.sums" (List.length u.checksums = 2)
   | None -> check "list.url" false);

  (* description with braces must not swallow the url section *)
  let braces =
    "opam-version: \"2.0\"\n\
     description: \"\"\"\n\
     records like { a = 1 } and stray braces } } {\n\
     \"\"\"\n\
     url {\n  src: \"https://example.org/pkg-1.0.tar.gz\"\n  checksum: \"sha256=abc\"\n}\n"
  in
  (match (Opamfile.parse braces).url with
   | Some u -> checkeq "braces.src" u.src "https://example.org/pkg-1.0.tar.gz"
   | None -> check "braces.url" false);

  (* extra-source before url: url must win, extra parsed separately *)
  let extra =
    "opam-version: \"2.0\"\n\
     extra-source \"fix.patch\" {\n  src: \"https://gist.example/raw/fix.patch\"\n  checksum: \"md5=bbbb\"\n}\n\
     url {\n  src: \"https://example.org/real-1.0.tar.gz\"\n  checksum: \"sha256=cafe\"\n}\n"
  in
  let f = Opamfile.parse extra in
  (match f.url with
   | Some u -> checkeq "extra.url" u.src "https://example.org/real-1.0.tar.gz"
   | None -> check "extra.url.present" false);
  check "extra.count" (List.length f.extra = 1);
  (match f.extra with
   | [ e ] ->
     checkeq "extra.name" e.name "fix.patch";
     checkeq "extra.src" e.src "https://gist.example/raw/fix.patch";
     check "extra.is_extra" (Opamfile.is_extra e)
   | _ -> check "extra.single" false);
  check "sources.count" (List.length (Opamfile.sources f) = 2);

  (* sourceless *)
  let none = Opamfile.parse "opam-version: \"2.0\"\ndepends: [\"conf-x\"]\n" in
  check "sourceless" (none.url = None);

  (* rewrite url only, preserve checksums and the rest *)
  let out = Opamfile.rewrite_src checksum_list "https://mirror/download/p/2.0.0/v2.0.0.tar.gz" in
  let g = Opamfile.parse out in
  (match g.url with
   | Some u ->
     checkeq "rw.src" u.src "https://mirror/download/p/2.0.0/v2.0.0.tar.gz";
     check "rw.sums" (List.length u.checksums = 2)
   | None -> check "rw.url" false);
  check "rw.no-upstream" (not (
    let re = Str.regexp_string "github.com/savonet" in
    (try ignore (Str.search_forward re out 0); true with Not_found -> false)));

  (* rewrite url + extras in one pass *)
  let out2 =
    Opamfile.rewrite_sources extra (Opamfile.parse extra) (fun s ->
        if Opamfile.is_extra s then Some ("https://mirror/x/" ^ s.name)
        else Some "https://mirror/x/main.tar.gz")
  in
  let h = Opamfile.parse out2 in
  (match h.url with Some u -> checkeq "rw2.url" u.src "https://mirror/x/main.tar.gz" | None -> check "rw2.url" false);
  (match h.extra with [ e ] -> checkeq "rw2.extra" e.src "https://mirror/x/fix.patch" | _ -> check "rw2.extra" false);

  (* skipping a source (None) leaves it untouched *)
  let out3 =
    Opamfile.rewrite_sources extra (Opamfile.parse extra) (fun s ->
        if Opamfile.is_extra s then None else Some "https://mirror/only.tar.gz")
  in
  let k = Opamfile.parse out3 in
  (match k.extra with [ e ] -> checkeq "skip.extra" e.src "https://gist.example/raw/fix.patch" | _ -> check "skip.extra" false);

  (* rewrite rejects a src with control chars *)
  check "reject.newline"
    (try ignore (Opamfile.rewrite_src checksum_list "https://mirror/x\n"); false
     with Opamfile.Rewrite_error _ -> true);

  if !failures = 0 then print_endline "opamfile: all tests passed"
  else (Printf.printf "opamfile: %d FAILURES\n" !failures; exit 1)

(* Corpus check: parse and round-trip every opam file in a real repository.
   Run with OPAMELA_CORPUS=/path/to/opam-repository dune exec test/test_opamela.exe *)
let read_file p =
  let ic = open_in_bin p in
  let n = in_channel_length ic in
  let s = really_input_string ic n in
  close_in ic; s

let () =
  match Sys.getenv_opt "OPAMELA_CORPUS" with
  | None -> ()
  | Some root ->
    let files = ref 0 and withurl = ref 0 and sourceless = ref 0 and fails = ref 0 in
    let packages = Filename.concat root "packages" in
    Array.iter (fun name ->
      let namedir = Filename.concat packages name in
      if Sys.is_directory namedir then
        Array.iter (fun ver ->
          let opam = Filename.concat (Filename.concat namedir ver) "opam" in
          if Sys.file_exists opam && not (Sys.is_directory opam) then begin
            incr files;
            let data = read_file opam in
            let f = Opamfile.parse data in
            match f.url with
            | None -> incr sourceless
            | Some u ->
              incr withurl;
              if String.contains u.src '\n' || String.contains u.src '"' then
                (incr fails; if !fails <= 5 then Printf.printf "FAIL %s: bad src\n" opam);
              let repl = "https://mirror.invalid/download/x/1.0/a.tar.gz" in
              let out = Opamfile.rewrite_sources data f (fun _ -> Some repl) in
              let g = Opamfile.parse out in
              (match g.url with
               | Some u2 when u2.src = repl -> ()
               | _ -> incr fails; if !fails <= 5 then Printf.printf "FAIL %s: roundtrip\n" opam)
          end)
          (Sys.readdir namedir))
      (Sys.readdir packages);
    Printf.printf "corpus: %d files, %d with a source, %d sourceless, %d failures\n"
      !files !withurl !sourceless !fails;
    if !fails > 0 then exit 1

(* --- mirror tests -------------------------------------------------------- *)
let mk_tmp () =
  let d = Filename.temp_file "opamela-test" "" in
  Sys.remove d; Unix.mkdir d 0o755; d

let write_pkg root name version content =
  let dir = Filename.concat (Filename.concat (Filename.concat root "packages") name)
              (name ^ "." ^ version) in
  Mirror.mkdir_p dir;
  Mirror.write_file (Filename.concat dir "opam") content

let () =
  let up = mk_tmp () in
  write_pkg up "pkg" "1.0"
    "opam-version: \"2.0\"\n\
     url {\n  src: \"https://upstream.example/pkg-1.0.tar.gz\"\n  checksum: \"sha256=0000\"\n}\n\
     extra-source \"fix.patch\" {\n  src: \"https://gist.example/raw/fix.patch\"\n  checksum: \"md5=0000\"\n}\n\
     extra-source \"over-git.patch\" {\n  src: \"git+https://example.org/thing.git\"\n}\n";
  write_pkg up "conf-x" "1" "opam-version: \"2.0\"\ndepends: [\"y\"]\n";
  let dst = mk_tmp () in
  (match Mirror.build ~base_url:"https://mirror.internal" { base = up; overlays = [] } dst with
   | Error e -> incr failures; Printf.printf "FAIL mirror.build: %s\n" e
   | Ok stats ->
     check "mirror.pkgs" (stats.packages = 2);
     check "mirror.rewritten" (stats.rewritten = 1);
     check "mirror.sourceless" (stats.sourceless = 1);
     (* the versioned opam file, rewritten *)
     let data = Mirror.read_file
         (Filename.concat dst "packages/pkg/pkg.1.0/opam") in
     let f = Opamfile.parse data in
     (match f.url with
      | Some u -> checkeq "mirror.url" u.src "https://mirror.internal/download/pkg/1.0/pkg-1.0.tar.gz"
      | None -> check "mirror.url.present" false);
     (match f.extra with
      | [ e1; e2 ] ->
        checkeq "mirror.extra.http" e1.src "https://mirror.internal/download/pkg/1.0/fix.patch";
        checkeq "mirror.extra.git" e2.src "git+https://example.org/thing.git"
      | _ -> check "mirror.extra.count" false);
     (* no stray file one level up, and the index exists *)
     check "mirror.no-stray" (not (Sys.file_exists (Filename.concat dst "packages/pkg/opam")));
     check "mirror.index" (Sys.file_exists (Filename.concat dst Mirror.index_name)));
  Mirror.rm_rf up; Mirror.rm_rf dst;

  (* archive naming edge cases *)
  checkeq "an.basic" (Mirror.archive_name "https://x.org/a/v1.0.tar.gz" "p" "1.0") "v1.0.tar.gz";
  checkeq "an.query" (Mirror.archive_name "https://x.org/a/v1.0.tar.gz?t=1" "p" "1.0") "v1.0.tar.gz";
  checkeq "an.fallback" (Mirror.archive_name "https://x.org/" "p" "1.0") "p.1.0.tar.gz";
  check "mirrorable.https" (Mirror.mirrorable "https://x.org/a.tgz");
  check "mirrorable.git" (not (Mirror.mirrorable "git+https://x.org/a.git"));
  check "mirrorable.ftp" (not (Mirror.mirrorable "ftp://x.org/a.tgz"));

  if !failures = 0 then print_endline "mirror: all tests passed"
  else (Printf.printf "mirror: %d FAILURES\n" !failures; exit 1)

(* Corpus mirror build: generate the whole mirror and compare with the Go
   implementation. OPAMELA_CORPUS must point at an opam-repository checkout. *)
let () =
  match Sys.getenv_opt "OPAMELA_CORPUS" with
  | None -> ()
  | Some root ->
    let dst = mk_tmp () in
    let t0 = Unix.gettimeofday () in
    (match Mirror.build ~base_url:"https://opam.internal" { base = root; overlays = [] } dst with
     | Error e -> Printf.printf "corpus-build FAIL: %s\n" e; exit 1
     | Ok s ->
       Printf.printf "corpus-build: %s in %.1fs\n" (Mirror.stats_to_string s)
         (Unix.gettimeofday () -. t0);
       (* count extra-sources that now point at the mirror *)
       let extras = ref 0 and extras_mirror = ref 0 and stray = ref 0 in
       let packages = Filename.concat dst "packages" in
       Array.iter (fun name ->
         let nd = Filename.concat packages name in
         if Sys.is_directory nd then begin
           if Sys.file_exists (Filename.concat nd "opam") then incr stray;
           Array.iter (fun ver ->
             let opam = Filename.concat (Filename.concat nd ver) "opam" in
             if Sys.file_exists opam && not (Sys.is_directory opam) then begin
               let f = Opamfile.parse (Mirror.read_file opam) in
               List.iter (fun (e : Opamfile.source) ->
                 incr extras;
                 let pfx = "https://opam.internal/download/" in
                 let starts s p = String.length s >= String.length p && String.sub s 0 (String.length p) = p in
                 if starts e.src "http://" || starts e.src "https://" then
                   if starts e.src pfx then incr extras_mirror
                   else (Printf.printf "extra not rewritten: %s\n" e.src))
                 f.extra
             end)
             (Sys.readdir nd)
         end)
         (Sys.readdir packages);
       Printf.printf "corpus-build: %d extra-sources, %d rewritten to the mirror\n" !extras !extras_mirror;
       if !stray > 0 then (Printf.printf "FAIL: %d stray opam files one level up\n" !stray; exit 1);
       Mirror.rm_rf dst)
