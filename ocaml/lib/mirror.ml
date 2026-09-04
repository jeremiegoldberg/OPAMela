(* Turn opam-repository checkouts into a repository whose packages point at this
   server. Port of the Go package. An opam repository is not a package store: it
   is a directory of URLs pointing at thousands of unrelated hosts. That is why a
   cache in front of one caches nothing worth caching, and why the only way to
   become the single path a build takes is to rewrite the directory itself. *)

let index_name = "index.tar.gz"

type stats = {
  packages : int;
  rewritten : int;
  sourceless : int;
  passthrough : int;
}

let stats_to_string s =
  Printf.sprintf "%d packages: %d rewritten, %d sourceless, %d passed through"
    s.packages s.rewritten s.sourceless s.passthrough

(* --- small helpers ------------------------------------------------------- *)

let rec mkdir_p dir =
  if dir <> "" && dir <> "." && dir <> "/" && not (Sys.file_exists dir) then begin
    mkdir_p (Filename.dirname dir);
    (try Unix.mkdir dir 0o755 with Unix.Unix_error (Unix.EEXIST, _, _) -> ())
  end

let read_file p =
  let ic = open_in_bin p in
  Fun.protect
    ~finally:(fun () -> close_in ic)
    (fun () ->
      let n = in_channel_length ic in
      really_input_string ic n)

let write_file p data =
  mkdir_p (Filename.dirname p);
  let oc = open_out_bin p in
  Fun.protect ~finally:(fun () -> close_out oc) (fun () -> output_string oc data)

let rec rm_rf p =
  match Sys.file_exists p with
  | false -> ()
  | true ->
      if (try Sys.is_directory p with Sys_error _ -> false) then begin
        Array.iter (fun e -> rm_rf (Filename.concat p e)) (Sys.readdir p);
        (try Unix.rmdir p with Unix.Unix_error _ -> ())
      end
      else try Sys.remove p with Sys_error _ -> ()

let run_shell cmd =
  match Unix.system cmd with
  | Unix.WEXITED 0 -> Ok ()
  | Unix.WEXITED n -> Error (Printf.sprintf "command exited %d: %s" n cmd)
  | _ -> Error (Printf.sprintf "command killed: %s" cmd)

(* --- url handling -------------------------------------------------------- *)

let scheme_of src =
  match String.index_opt src ':' with
  | Some i when i + 2 < String.length src && src.[i + 1] = '/' && src.[i + 2] = '/'
    ->
      Some (String.sub src 0 i, String.sub src (i + 3) (String.length src - i - 3))
  | _ -> None

(* whether a source URL can be served as a plain file *)
let mirrorable src =
  match scheme_of src with
  | Some (("http" | "https"), rest) ->
      (match String.index_opt rest '/' with
      | Some i -> i > 0 && i < String.length rest - 1
      | None -> false)
  | _ -> false

let safe_archive_name s =
  if String.length s = 0 || String.length s > 200 then false
  else begin
    let has_dotdot =
      let bad = ref false in
      for i = 0 to String.length s - 2 do
        if s.[i] = '.' && s.[i + 1] = '.' then bad := true
      done;
      !bad
    in
    if has_dotdot then false
    else begin
      let ok = ref true in
      String.iter
        (fun c ->
          match c with
          | 'a' .. 'z' | 'A' .. 'Z' | '0' .. '9' | '.' | '-' | '_' | '+' | '~' -> ()
          | _ -> ok := false)
        s;
      !ok
    end
  end

(* the last path element of a URL, host and query stripped *)
let url_basename src =
  match scheme_of src with
  | None -> ""
  | Some (_, rest) ->
      let rest =
        match String.index_opt rest '?' with
        | Some q -> String.sub rest 0 q
        | None -> rest
      in
      (* rest is host[/path]; keep only from the first '/' *)
      let path =
        match String.index_opt rest '/' with
        | Some i -> String.sub rest i (String.length rest - i)
        | None -> ""
      in
      if path = "" then "" else Filename.basename path

let archive_name src pkg version =
  let fallback = pkg ^ "." ^ version ^ ".tar.gz" in
  let name = url_basename src in
  if name = "" || name = "." || name = "/" || name = ".." then fallback
  else if not (safe_archive_name name) then fallback
  else name

(* The archive name a rewritten source advertises. For the url section it comes
   from the upstream URL; for an extra-source it is its label, which opam
   guarantees unique within a package. *)
let archive_name_for (s : Opamfile.source) pkg version =
  if Opamfile.is_extra s && safe_archive_name s.name then s.name
  else archive_name s.src pkg version

let download_path pkg version archive =
  "/download/" ^ pkg ^ "/" ^ version ^ "/" ^ archive

(* --- building ------------------------------------------------------------ *)

(* copy everything in a package directory except its opam file *)
let rec copy_extras src_dir dst_dir =
  Array.iter
    (fun entry ->
      if entry <> "opam" then begin
        let s = Filename.concat src_dir entry in
        let d = Filename.concat dst_dir entry in
        if (try Sys.is_directory s with Sys_error _ -> false) then begin
          mkdir_p d;
          copy_extras s d
        end
        else if (Unix.lstat s).Unix.st_kind = Unix.S_REG then
          write_file d (read_file s)
      end)
    (Sys.readdir src_dir)

type kind = Rewritten | Sourceless | Passthrough

let build_package (pkg : Opamrepo.package) dst_root base : kind =
  let dst_dir = Filename.concat dst_root (Opamrepo.rel_dir pkg) in
  mkdir_p dst_dir;
  copy_extras pkg.dir dst_dir;

  let data = read_file (Opamrepo.opam_path pkg) in
  let f = Opamfile.parse data in

  (* Rewrite the url source and every mirrorable extra-source in one pass, so
     the patches opam fetches at build time come through the mirror too. *)
  let out =
    Opamfile.rewrite_sources data f (fun (s : Opamfile.source) ->
        if not (mirrorable s.src) then None
        else
          Some (base ^ download_path pkg.name pkg.version (archive_name_for s pkg.name pkg.version)))
  in

  let kind =
    match f.url with
    | None -> Sourceless
    | Some u -> if mirrorable u.src then Rewritten else Passthrough
  in
  write_file (Filename.concat dst_dir "opam") out;
  kind

let copy_repo_file base dst_root =
  let repo = if base = "" then "" else Filename.concat base "repo" in
  let data =
    if repo <> "" && Sys.file_exists repo then read_file repo
    else "opam-version: \"2.0\"\n"
  in
  write_file (Filename.concat dst_root "repo") data

(* Pack the generated tree into index.tar.gz, deterministically: sorted names,
   zeroed timestamps and ownership, gzip without a name/timestamp header. Two
   builds of the same input therefore produce identical tarballs, which makes
   "did the mirror actually change?" a question with an answer. Shelling out to
   tar mirrors how the Go version leans on the standard archive/tar; OCaml's
   stdlib has neither tar nor gzip. *)
let write_index dst_root =
  let cmd =
    Printf.sprintf
      "cd %s && find . -mindepth 1 ! -name %s | LC_ALL=C sort | \
       tar --owner=0 --group=0 --numeric-owner --mtime=@0 --no-recursion \
       -T - -cf - | gzip -n > %s"
      (Filename.quote dst_root) (Filename.quote index_name)
      (Filename.quote index_name)
  in
  run_shell cmd

(* Write a rewritten copy of the source repositories into dst_root, replacing
   whatever was there, and generate the index tarball. base_url is the public
   root this server answers on, without a trailing slash. *)
let build ~base_url (src : Opamrepo.sources) dst_root : (stats, string) result =
  let base_url =
    if String.length base_url > 0 && base_url.[String.length base_url - 1] = '/' then
      String.sub base_url 0 (String.length base_url - 1)
    else base_url
  in
  if base_url = "" then Error "mirror: empty base_url"
  else
    let roots = Opamrepo.roots src in
    if roots = [] then Error "mirror: no source repository"
    else begin
      let packages = Opamrepo.list roots in
      rm_rf dst_root;
      mkdir_p dst_root;
      copy_repo_file src.base dst_root;
      let rewritten = ref 0 and sourceless = ref 0 and passthrough = ref 0 in
      List.iter
        (fun pkg ->
          match build_package pkg dst_root base_url with
          | Rewritten -> incr rewritten
          | Sourceless -> incr sourceless
          | Passthrough -> incr passthrough)
        packages;
    match write_index dst_root with
    | Error e -> Error e
    | Ok () ->
        Ok
          {
            packages = List.length packages;
            rewritten = !rewritten;
            sourceless = !sourceless;
            passthrough = !passthrough;
          }
  end
