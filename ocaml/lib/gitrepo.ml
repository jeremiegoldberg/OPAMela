(* Keep a shallow checkout of a remote git repository in sync, by shelling out
   to git. Port of the Go package: git is already a hard requirement of any
   machine that builds OCaml, so a shallow clone is one process, not a
   dependency. *)

type t = { url : string; dir : string }

(* run a shell command with git's interactive prompts disabled, capturing
   stdout and stderr together. stderr is folded into stdout so a large clone's
   progress cannot fill a pipe we are not draining. *)
let capture cmd =
  let full =
    Printf.sprintf "GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=true %s 2>&1" cmd
  in
  let ic = Unix.open_process_in full in
  let b = Buffer.create 256 in
  (try
     while true do
       Buffer.add_channel b ic 4096
     done
   with End_of_file -> ());
  let status = Unix.close_process_in ic in
  (status, Buffer.contents b)

let ok = function Unix.WEXITED 0 -> true | _ -> false

let head t : (string, string) result =
  match capture (Printf.sprintf "git -C %s rev-parse HEAD" (Filename.quote t.dir)) with
  | status, out when ok status -> Ok (String.trim out)
  | _, out -> Error (String.trim out)

(* Make the checkout exist and match the remote's default branch, then report
   the revision. Safe to call repeatedly: the first call clones, later calls
   fetch and hard-reset. A hard reset is right because the checkout is a cache,
   not a workspace: nothing local is ever worth keeping. *)
let sync t : (string, string) result =
  let git_dir = Filename.concat t.dir ".git" in
  if not (Sys.file_exists git_dir) then begin
    Mirror.mkdir_p (Filename.dirname t.dir);
    Mirror.rm_rf t.dir;
    match
      capture
        (Printf.sprintf "git clone --depth 1 --single-branch %s %s"
           (Filename.quote t.url) (Filename.quote t.dir))
    with
    | status, _ when ok status -> head t
    | _, out -> Error (Printf.sprintf "cloning %s: %s" t.url (String.trim out))
  end
  else begin
    let step cmd = capture (Printf.sprintf "git -C %s %s" (Filename.quote t.dir) cmd) in
    (* Fetching HEAD by name avoids having to know the default branch, which
       differs between opam-repository and the overlay repositories. *)
    match step "fetch --depth 1 origin HEAD" with
    | status, out when not (ok status) ->
        Error (Printf.sprintf "fetching %s: %s" t.url (String.trim out))
    | _ -> (
        match step "reset --hard FETCH_HEAD" with
        | status, out when not (ok status) ->
            Error (Printf.sprintf "resetting %s: %s" t.dir (String.trim out))
        | _ -> (
            match step "clean -fdx" with
            | status, out when not (ok status) ->
                Error (Printf.sprintf "cleaning %s: %s" t.dir (String.trim out))
            | _ -> head t))
  end
