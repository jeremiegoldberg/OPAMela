(* Parse and rewrite the downloadable sources of an opam package file.

   This is the OCaml port of the Go package of the same name. It implements
   only as much of the opam file format as a mirror needs: the top-level [url]
   section and every [extra-source] section, their [src] and [checksum], and
   replacing each [src] in place without disturbing another byte.

   Parse never fails. A file it cannot make sense of yields no sources, and a
   mirror leaves such a file untouched rather than choking on it: one malformed
   package upstream must not take down a rebuild of twenty thousand. *)

type checksum = { kind : string; hex : string }

let checksum_to_string c = c.kind ^ "=" ^ c.hex

(* One downloadable archive: the url section, or one extra-source. opam fetches
   extra-sources (patches and the like) separately at build time, so a mirror
   that ignores them leaves a hole through which builds still reach the net. *)
type source = {
  name : string;             (* "" for the url section; the label of an extra-source *)
  src : string;
  checksums : checksum list;
  start_ : int;              (* byte range of the src literal, quotes included *)
  end_ : int;
}

let is_extra (s : source) = s.name <> ""

type t = { url : source option; extra : source list }

let sources (t : t) : source list =
  (match t.url with Some u -> [ u ] | None -> []) @ t.extra

(* --- scanner ------------------------------------------------------------- *)

type scanner = { data : string; hi : int; mutable pos : int }

let eof s = s.pos >= s.hi
let peek s = s.data.[s.pos]

let is_word = function
  | 'a' .. 'z' | 'A' .. 'Z' | '0' .. '9' | '-' | '_' | '+' -> true
  | _ -> false

(* consume whitespace and comments, newlines included: opam wraps long values
   onto the following line freely. *)
let skip_trivia s =
  let go = ref true in
  while !go && not (eof s) do
    match peek s with
    | ' ' | '\t' | '\n' | '\r' -> s.pos <- s.pos + 1
    | '#' ->
        while (not (eof s)) && peek s <> '\n' do
          s.pos <- s.pos + 1
        done
    | _ -> go := false
  done

let read_ident s =
  let start = s.pos in
  while (not (eof s)) && is_word (peek s) do
    s.pos <- s.pos + 1
  done;
  String.sub s.data start (s.pos - start)

let has_prefix_at s i lit =
  i + String.length lit <= s.hi && String.sub s.data i (String.length lit) = lit

(* read a string literal, returning its unescaped value and the byte range it
   occupies, quotes included. Handles both "..." and the triple-quoted form
   used by description fields, which is what keeps brace counting honest. *)
let read_string s : (string * int * int) option =
  if eof s || peek s <> '"' then None
  else begin
    let start = s.pos in
    if has_prefix_at s s.pos "\"\"\"" then begin
      s.pos <- s.pos + 3;
      let rec find i =
        if i + 3 > s.hi then None
        else if String.sub s.data i 3 = "\"\"\"" then Some i
        else find (i + 1)
      in
      match find s.pos with
      | None ->
          s.pos <- s.hi;
          None
      | Some i ->
          let v = String.sub s.data s.pos (i - s.pos) in
          s.pos <- i + 3;
          Some (v, start, s.pos)
    end
    else begin
      s.pos <- s.pos + 1;
      (* opening quote *)
      let b = Buffer.create 32 in
      let result = ref None in
      let go = ref true in
      while !go && not (eof s) do
        let c = peek s in
        if c = '\\' && s.pos + 1 < s.hi then begin
          s.pos <- s.pos + 1;
          (match s.data.[s.pos] with
          | 'n' -> Buffer.add_char b '\n'
          | 't' -> Buffer.add_char b '\t'
          | 'r' -> Buffer.add_char b '\r'
          | e -> Buffer.add_char b e);
          s.pos <- s.pos + 1
        end
        else if c = '"' then begin
          s.pos <- s.pos + 1;
          result := Some (Buffer.contents b, start, s.pos);
          go := false
        end
        else begin
          Buffer.add_char b c;
          s.pos <- s.pos + 1
        end
      done;
      !result
    end
  end

let parse_checksum str : checksum option =
  let str = String.trim str in
  match String.index_opt str '=' with
  | None -> None
  | Some i ->
      let kind = String.sub str 0 i in
      let hex = String.sub str (i + 1) (String.length str - i - 1) in
      (match kind with
      | "md5" | "sha256" | "sha512" ->
          if hex = "" then None
          else Some { kind; hex = String.lowercase_ascii hex }
      | _ -> None)

(* read either a single "kind=hex" literal or a bracketed list of them. *)
let read_checksums s : checksum list =
  skip_trivia s;
  if eof s then []
  else if peek s <> '[' then
    match read_string s with
    | Some (lit, _, _) -> ( match parse_checksum lit with Some c -> [ c ] | None -> [] )
    | None -> []
  else begin
    s.pos <- s.pos + 1;
    (* '[' *)
    let acc = ref [] in
    let go = ref true in
    while !go && not (eof s) do
      skip_trivia s;
      if eof s || peek s = ']' then begin
        if not (eof s) then s.pos <- s.pos + 1;
        go := false
      end
      else
        match read_string s with
        | None -> s.pos <- s.pos + 1
        | Some (lit, _, _) -> (
            match parse_checksum lit with Some c -> acc := c :: !acc | None -> ())
    done;
    List.rev !acc
  end

let rec skip_balanced s op cl =
  if (not (eof s)) && peek s = op then begin
    let depth = ref 0 in
    let go = ref true in
    while !go && not (eof s) do
      let c = peek s in
      if c = '"' then ignore (read_string s)
      else begin
        if c = '#' then
          while (not (eof s)) && peek s <> '\n' do
            s.pos <- s.pos + 1
          done
        else begin
          if c = op then incr depth
          else if c = cl then begin
            decr depth;
            if !depth = 0 then begin
              s.pos <- s.pos + 1;
              go := false
            end
          end;
          if !go then s.pos <- s.pos + 1
        end
      end
    done
  end

and skip_value s =
  skip_trivia s;
  if not (eof s) then begin
    (match peek s with
    | '"' -> ignore (read_string s)
    | '[' -> skip_balanced s '[' ']'
    | '{' -> skip_balanced s '{' '}'
    | _ ->
        (* a bare token: identifier, number, boolean or a small expression such
           as [os != "win32"]; these do not span lines. *)
        while (not (eof s)) && peek s <> '\n' && peek s <> '#' do
          s.pos <- s.pos + 1
        done);
    skip_trivia s;
    if (not (eof s)) && peek s = '{' then skip_balanced s '{' '}'
  end

(* call [fn name label body_start body_end] for every top-level section. Walking
   the top level rather than searching for "url {" matters: those tokens also
   appear inside strings and descriptions, where a regex finds phantoms. *)
let walk_sections data fn =
  let s = { data; hi = String.length data; pos = 0 } in
  let go = ref true in
  while !go do
    skip_trivia s;
    if eof s then go := false
    else begin
      let ident = read_ident s in
      if ident = "" then s.pos <- s.pos + 1
      else begin
        skip_trivia s;
        if eof s then go := false
        else
          match peek s with
          | ':' ->
              s.pos <- s.pos + 1;
              skip_value s
          | '{' ->
              let op = s.pos in
              skip_balanced s '{' '}';
              fn ident "" (op + 1) (s.pos - 1)
          | '"' -> (
              match read_string s with
              | Some (label, _, _) ->
                  skip_trivia s;
                  if (not (eof s)) && peek s = '{' then begin
                    let op = s.pos in
                    skip_balanced s '{' '}';
                    fn ident label (op + 1) (s.pos - 1)
                  end
              | None -> s.pos <- s.pos + 1)
          | _ -> s.pos <- s.pos + 1
      end
    end
  done

let parse_source data body_start body_end label : source option =
  let s = { data; hi = body_end; pos = body_start } in
  let src = ref "" and rng = ref (0, 0) and sums = ref [] in
  let go = ref true in
  while !go do
    skip_trivia s;
    if eof s then go := false
    else begin
      let name = read_ident s in
      if name = "" then s.pos <- s.pos + 1
      else begin
        skip_trivia s;
        if eof s || peek s <> ':' then ()
        else begin
          s.pos <- s.pos + 1;
          skip_trivia s;
          match name with
          | "src" | "archive" -> (
              match read_string s with
              | Some (lit, a, b) ->
                  src := lit;
                  rng := (a, b)
              | None -> ())
          | "checksum" -> sums := !sums @ read_checksums s
          | _ -> skip_value s
        end
      end
    end
  done;
  if !src = "" then None
  else
    let a, b = !rng in
    Some { name = label; src = !src; checksums = !sums; start_ = a; end_ = b }

let parse (data : string) : t =
  let url = ref None and extra = ref [] in
  walk_sections data (fun name label body_start body_end ->
      match name with
      | "url" -> (
          if !url = None then
            match parse_source data body_start body_end "" with
            | Some s -> url := Some s
            | None -> ())
      | "extra-source" when label <> "" -> (
          match parse_source data body_start body_end label with
          | Some s -> extra := s :: !extra
          | None -> ())
      | _ -> ());
  { url = !url; extra = List.rev !extra }

exception Rewrite_error of string

let has_forbidden v =
  let bad = ref false in
  String.iter
    (fun c -> match c with '"' | '\\' | '\n' | '\r' | '\t' -> bad := true | _ -> ())
    v;
  !bad

(* Return a copy of [data] where the src of each source is replaced by the value
   [replace] returns (Some v), leaving alone the ones it maps to None. Every
   other byte is preserved. A replacement carrying a quote, backslash or control
   character is refused: it is the surest way to silently produce a repository
   that parses but resolves to nothing. *)
let rewrite_sources (data : string) (t : t) (replace : source -> string option) :
    string =
  let repls = ref [] in
  let add (s : source) =
    match replace s with
    | None -> ()
    | Some v ->
        if v = "" then ()
        else if has_forbidden v then
          raise (Rewrite_error (Printf.sprintf "refusing to write src %S" v))
        else repls := (s.start_, s.end_, v) :: !repls
  in
  (match t.url with Some u -> add u | None -> ());
  List.iter add t.extra;
  match !repls with
  | [] -> data
  | repls ->
      let repls = List.sort (fun (a, _, _) (b, _, _) -> compare a b) repls in
      let b = Buffer.create (String.length data + 64) in
      let prev = ref 0 in
      List.iter
        (fun (start_, end_, v) ->
          Buffer.add_substring b data !prev (start_ - !prev);
          Buffer.add_char b '"';
          Buffer.add_string b v;
          Buffer.add_char b '"';
          prev := end_)
        repls;
      Buffer.add_substring b data !prev (String.length data - !prev);
      Buffer.contents b

(* Rewrite only the url section's src, leaving extra-sources alone. *)
let rewrite_src (data : string) (new_src : string) : string =
  let t = parse data in
  match t.url with
  | None -> raise (Rewrite_error "no url.src")
  | Some _ ->
      if new_src = "" then raise (Rewrite_error "empty src");
      rewrite_sources data t (fun s -> if is_extra s then None else Some new_src)
