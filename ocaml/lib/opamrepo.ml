(* Locate packages inside one or more opam-repository checkouts. Port of the Go
   package: the version comes from the directory name, which is where opam reads
   it from, and overlays are searched before the base repository. *)

type package = { name : string; version : string; dir : string }

let opam_path (p : package) = Filename.concat p.dir "opam"

let rel_dir (p : package) =
  Filename.concat (Filename.concat "packages" p.name) (p.name ^ "." ^ p.version)

(* The set of repositories a mirror is built from. Overlays win over base, which
   is what makes opam-monorepo usable through a mirror: dune-universe overlays
   redefine packages to build with dune, and those must take precedence. *)
type sources = { base : string; overlays : string list }

let roots (s : sources) : string list =
  s.overlays @ (if s.base = "" then [] else [ s.base ])

let valid_element what s =
  if s = "" then Error (Printf.sprintf "empty %s" what)
  else if String.length s > 128 then Error (what ^ " too long")
  else if s = "." || s = ".." || String.contains s '/' || String.contains s '\\'
  then Error (Printf.sprintf "invalid %s %S" what s)
  else begin
    let ok = ref true in
    String.iter
      (fun c ->
        match c with
        | 'a' .. 'z' | 'A' .. 'Z' | '0' .. '9' | '.' | '-' | '_' | '+' | '~' -> ()
        | _ -> ok := false)
      s;
    if !ok then Ok () else Error (Printf.sprintf "invalid character in %s %S" what s)
  end

let valid_name = valid_element "package name"
let valid_version = valid_element "version"

let version_of name dirname =
  let prefix = name ^ "." in
  let pl = String.length prefix in
  if String.length dirname > pl && String.sub dirname 0 pl = prefix then
    Some (String.sub dirname pl (String.length dirname - pl))
  else None

let is_dir p = try Sys.is_directory p with Sys_error _ -> false
let has_opam dir = Sys.file_exists (Filename.concat dir "opam")

(* Every versioned package visible across roots. Roots are searched in order and
   the first to provide a given name.version wins, so a later root never shadows
   an earlier one. *)
let list (roots : string list) : package list =
  let seen = Hashtbl.create 4096 in
  let acc = ref [] in
  List.iter
    (fun root ->
      let packages = Filename.concat root "packages" in
      if is_dir packages then
        Array.iter
          (fun name ->
            let namedir = Filename.concat packages name in
            if is_dir namedir then
              Array.iter
                (fun base ->
                  match version_of name base with
                  | None -> ()
                  | Some version ->
                      if not (Hashtbl.mem seen base) then begin
                        let dir = Filename.concat namedir base in
                        if has_opam dir then begin
                          Hashtbl.add seen base ();
                          acc := { name; version; dir } :: !acc
                        end
                      end)
                (Sys.readdir namedir))
          (Sys.readdir packages))
    roots;
  List.rev !acc

(* Resolve one package by name and version, searching roots in order. These
   values arrive from HTTP request paths, so this is the boundary between a
   request and the filesystem. *)
let find (roots : string list) name version : (package, string) result =
  match valid_name name with
  | Error e -> Error e
  | Ok () -> (
      match valid_version version with
      | Error e -> Error e
      | Ok () ->
          let base = name ^ "." ^ version in
          let rec go = function
            | [] -> Error "not found"
            | root :: rest ->
                let dir =
                  Filename.concat
                    (Filename.concat (Filename.concat root "packages") name)
                    base
                in
                if has_opam dir then Ok { name; version; dir } else go rest
          in
          go roots)
