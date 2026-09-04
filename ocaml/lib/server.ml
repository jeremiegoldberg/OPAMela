(* Expose the mirror over HTTP. Port of the Go package, with a small hand-written
   HTTP/1.1 server (one thread per connection, Connection: close) since OCaml's
   stdlib ships no HTTP. It answers exactly what opam asks for: the repository
   files, the index tarball, and /download archives fetched on demand. *)

type state = {
  mirror_dir : string;
  pristine : Opamrepo.sources;
  rev : string;
  built_at : string;
}

type t = { cache : Fetch.t; log : string -> unit; mutable st : state option }

let create ?(log = fun _ -> ()) cache = { cache; log; st = None }

(* Publishing a freshly built pair of trees is a single field store, so a
   rebuild never exposes a half-written repository. *)
let set_state t s = t.st <- Some s

(* --- socket read/write --------------------------------------------------- *)

type reader = { fd : Unix.file_descr; buf : Bytes.t; mutable len : int; mutable pos : int }

let reader fd = { fd; buf = Bytes.create 4096; len = 0; pos = 0 }

let read_byte r =
  if r.pos >= r.len then begin
    r.pos <- 0;
    r.len <- (try Unix.read r.fd r.buf 0 (Bytes.length r.buf) with Unix.Unix_error _ -> 0)
  end;
  if r.len = 0 then None
  else begin
    let c = Bytes.get r.buf r.pos in
    r.pos <- r.pos + 1;
    Some c
  end

let read_line r =
  let b = Buffer.create 128 in
  let rec loop () =
    match read_byte r with
    | None -> if Buffer.length b = 0 then None else Some (Buffer.contents b)
    | Some '\n' -> Some (Buffer.contents b)
    | Some c -> Buffer.add_char b c; loop ()
  in
  match loop () with
  | None -> None
  | Some s ->
      let n = String.length s in
      Some (if n > 0 && s.[n - 1] = '\r' then String.sub s 0 (n - 1) else s)

let write_all fd s =
  let n = String.length s in
  let off = ref 0 in
  while !off < n do
    off := !off + Unix.write_substring fd s !off (n - !off)
  done

let respond fd ?(status = "200 OK") ?(ctype = "text/plain; charset=utf-8") body =
  write_all fd
    (Printf.sprintf
       "HTTP/1.1 %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s"
       status ctype (String.length body) body)

let serve_file fd ?(ctype = "application/octet-stream") path =
  let ic = open_in_bin path in
  Fun.protect
    ~finally:(fun () -> close_in ic)
    (fun () ->
      let n = in_channel_length ic in
      write_all fd
        (Printf.sprintf
           "HTTP/1.1 200 OK\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n"
           ctype n);
      let buf = Bytes.create 65536 in
      let rec loop () =
        let r = input ic buf 0 (Bytes.length buf) in
        if r > 0 then begin
          let off = ref 0 in
          while !off < r do
            off := !off + Unix.write fd buf !off (r - !off)
          done;
          loop ()
        end
      in
      loop ())

(* --- routing helpers ----------------------------------------------------- *)

let split_segments p =
  List.filter (fun s -> s <> "") (String.split_on_char '/' p)

(* reduce a request path to a safe relative path, or None if it tries to escape *)
let clean_request_path p =
  let parts = String.split_on_char '/' p in
  let rec go acc = function
    | [] -> Some (List.rev acc)
    | ("" | ".") :: rest -> go acc rest
    | ".." :: _ -> None
    | seg :: rest -> go (seg :: acc) rest
  in
  match go [] parts with Some segs -> Some (String.concat "/" segs) | None -> None

let is_regular_file p =
  try (Unix.stat p).Unix.st_kind = Unix.S_REG with Unix.Unix_error _ -> false

let handle_download t (st : state) fd pkg version archive =
  match Opamrepo.find (Opamrepo.roots st.pristine) pkg version with
  | Error _ -> respond fd ~status:"404 Not Found" "not found\n"
  | Ok pkgrec -> (
      let data = Mirror.read_file (Opamrepo.opam_path pkgrec) in
      let f = Opamfile.parse data in
      (* resolve the archive back to one of the package's sources; the name is
         recomputed and matched, never trusted from the request. *)
      let matched =
        List.find_opt
          (fun (s : Opamfile.source) ->
            Mirror.mirrorable s.src
            && Mirror.archive_name_for s pkgrec.name pkgrec.version = archive)
          (Opamfile.sources f)
      in
      match matched with
      | None -> respond fd ~status:"404 Not Found" "not found\n"
      | Some src -> (
          let key = pkgrec.name ^ "/" ^ pkgrec.version ^ "/" ^ archive in
          try
            match Fetch.get t.cache key src.src src.checksums with
            | Ok local -> serve_file fd local
            | Error msg ->
                t.log (Printf.sprintf "fetching %s: %s" key msg);
                respond fd ~status:"502 Bad Gateway" "upstream fetch failed\n"
          with Fetch.Checksum_mismatch msg ->
            t.log (Printf.sprintf "refusing to serve %s: %s" key msg);
            respond fd ~status:"502 Bad Gateway"
              "upstream archive failed checksum verification\n"))

let handle_repo (st : state) fd path =
  match clean_request_path path with
  | None -> respond fd ~status:"404 Not Found" "not found\n"
  | Some "" ->
      respond fd
        "opamela: an opam repository mirror\n\n  opam repository set-url default <this-url>\n"
  | Some rel ->
      let full =
        Filename.concat st.mirror_dir
          (String.concat Filename.dir_sep (String.split_on_char '/' rel))
      in
      (* directory listings are of no use to opam and only invite crawlers *)
      if is_regular_file full then serve_file fd ~ctype:"text/plain; charset=utf-8" full
      else respond fd ~status:"404 Not Found" "not found\n"

let handle t fd =
  Fun.protect
    ~finally:(fun () -> try Unix.close fd with Unix.Unix_error _ -> ())
    (fun () ->
      let r = reader fd in
      match read_line r with
      | None -> ()
      | Some request_line ->
          (* drain the remaining request headers *)
          let rec drain () =
            match read_line r with Some "" | None -> () | Some _ -> drain ()
          in
          drain ();
          let meth, target =
            match String.split_on_char ' ' request_line with
            | m :: tgt :: _ -> (m, tgt)
            | _ -> ("", "")
          in
          let path =
            match String.index_opt target '?' with
            | Some i -> String.sub target 0 i
            | None -> target
          in
          if meth <> "GET" then respond fd ~status:"405 Method Not Allowed" "method not allowed\n"
          else
            match t.st with
            | None -> respond fd ~status:"503 Service Unavailable" "mirror is still building\n"
            | Some st -> (
                if path = "/healthz" then
                  respond fd (Printf.sprintf "ok rev=%s built=%s\n" st.rev st.built_at)
                else
                  match split_segments path with
                  | "download" :: pkg :: version :: rest when rest <> [] ->
                      let archive = String.concat "/" rest in
                      handle_download t st fd pkg version archive
                  | _ -> handle_repo st fd path))

(* --- listening ----------------------------------------------------------- *)

let parse_addr a =
  match String.rindex_opt a ':' with
  | None -> failwith (Printf.sprintf "invalid listen address %S (want host:port)" a)
  | Some i ->
      let host = String.sub a 0 i in
      let port = int_of_string (String.sub a (i + 1) (String.length a - i - 1)) in
      let inet =
        if host = "" || host = "*" then Unix.inet_addr_any
        else Unix.inet_addr_of_string host
      in
      Unix.ADDR_INET (inet, port)

let bind addr =
  let s = Unix.socket Unix.PF_INET Unix.SOCK_STREAM 0 in
  Unix.setsockopt s Unix.SO_REUSEADDR true;
  Unix.bind s addr;
  Unix.listen s 128;
  s

let bound_port s =
  match Unix.getsockname s with Unix.ADDR_INET (_, p) -> p | _ -> 0

(* accept connections forever, one thread per connection *)
let serve t sock =
  while true do
    match Unix.accept sock with
    | c, _ -> ignore (Thread.create (fun () -> try handle t c with _ -> ()) ())
    | exception Unix.Unix_error (Unix.EINTR, _, _) -> ()
  done
