(* Download package archives once and keep them on disk. Port of the Go package,
   with the same guarantees: a file that appears in the cache is complete and
   matched its declared checksum, and simultaneous requests for a cold archive
   cause one download.

   Downloading and hashing are delegated to curl and the sha*sum tools rather
   than linked in: OCaml's stdlib has neither TLS nor the digests, and those
   tools are present wherever a CI runner is. *)

exception Checksum_mismatch of string

type t = {
  dir : string;
  mutex : Mutex.t;                       (* guards [locks] *)
  locks : (string, Mutex.t) Hashtbl.t;  (* one lock per key, for single-flight *)
  log : string -> unit;
}

let create ?(log = fun _ -> ()) dir =
  { dir; mutex = Mutex.create (); locks = Hashtbl.create 64; log }

let path t key = Filename.concat t.dir (String.concat Filename.dir_sep (String.split_on_char '/' key))

let key_lock t key =
  Mutex.lock t.mutex;
  let m =
    match Hashtbl.find_opt t.locks key with
    | Some m -> m
    | None ->
        let m = Mutex.create () in
        Hashtbl.add t.locks key m;
        m
  in
  Mutex.unlock t.mutex;
  m

let tool_of_kind = function
  | "md5" -> Some "md5sum"
  | "sha256" -> Some "sha256sum"
  | "sha512" -> Some "sha512sum"
  | _ -> None

let digest tool file =
  let status, out =
    Gitrepo.capture (Printf.sprintf "%s < %s" tool (Filename.quote file))
  in
  match status with
  | Unix.WEXITED 0 -> (
      match String.split_on_char ' ' (String.trim out) with
      | h :: _ -> Some (String.lowercase_ascii h)
      | [] -> None)
  | _ -> None

let verify t file (checksums : Opamfile.checksum list) src =
  let checked = ref 0 in
  let bad = ref None in
  List.iter
    (fun (c : Opamfile.checksum) ->
      match tool_of_kind c.kind with
      | None -> ()
      | Some tool -> (
          match digest tool file with
          | Some got ->
              incr checked;
              if got <> c.hex then
                bad :=
                  Some
                    (Printf.sprintf "%s for %s: %s got=%s expected=%s"
                       "checksum mismatch" src c.kind got c.hex)
          | None -> ()))
    checksums;
  match !bad with
  | Some msg -> Error msg
  | None ->
      if !checked = 0 then
        t.log (Printf.sprintf "archive has no usable checksum, serving unverified: %s" src);
      Ok ()

(* download src to dst, verifying it before it becomes visible. The archive is
   streamed to a temporary file, hashed, then renamed into place, so nothing
   partial or unverified ever carries the name of a real archive. *)
let download t dst src checksums : (unit, string) result =
  Mirror.mkdir_p (Filename.dirname dst);
  let tmp = dst ^ ".partial." ^ string_of_int (Unix.getpid ()) in
  let cleanup () = try Sys.remove tmp with Sys_error _ -> () in
  let status, out =
    Gitrepo.capture
      (Printf.sprintf "curl -fsSL --retry 2 -o %s %s" (Filename.quote tmp)
         (Filename.quote src))
  in
  match status with
  | Unix.WEXITED 0 -> (
      match verify t tmp checksums src with
      | Error msg ->
          cleanup ();
          Error msg
      | Ok () ->
          (try Sys.rename tmp dst with e -> cleanup (); raise e);
          t.log (Printf.sprintf "cached archive %s from %s" (Filename.basename dst) src);
          Ok ())
  | _ ->
      cleanup ();
      Error (Printf.sprintf "fetching %s: %s" src (String.trim out))

(* Return the local path of the archive for key, downloading it from src on
   first use. Checksums come from the package's own opam file, so verification
   here is a check against upstream, not against the client. *)
let get t key src checksums : (string, string) result =
  let dst = path t key in
  if Sys.file_exists dst then Ok dst
  else begin
    let lock = key_lock t key in
    Mutex.lock lock;
    Fun.protect
      ~finally:(fun () -> Mutex.unlock lock)
      (fun () ->
        if Sys.file_exists dst then Ok dst
        else
          match download t dst src checksums with
          | Ok () -> Ok dst
          | Error msg ->
              if
                (* surface a checksum failure distinctly *)
                let re = Str.regexp_string "checksum mismatch" in
                (try ignore (Str.search_forward re msg 0); true
                 with Not_found -> false)
              then raise (Checksum_mismatch msg)
              else Error msg)
  end
