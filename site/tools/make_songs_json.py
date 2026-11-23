#!/usr/bin/env python3
"""Make site/songs.json, the song names the stats page shows for GoCentral's song IDs.

GoCentral stores numeric song IDs only. This reads Rock Band 3 song metadata in
text DTA (songs.dta) and writes {"<song_id>": {"name": ..., "artist": ...}} for
every real song in it: entries with their own name, artist and numeric song_id,
leaving out fakes (lessons, test songs) and anything under #ifndef _SHIP.

Sources, any number, later ones winning for the same ID:
  - a text songs.dta: an official DLC package's songs/songs.dta, or the disc's
    songs/gen/songs.dtb printed as text by band3_recomp's
    tools/read_game_config.py --print songs/gen/songs.dtb
  - --band3-recomp PATH: a band3_recomp checkout with its assets/gen archive;
    the disc's songs/gen/songs.dtb is read through its tools/read_game_config.py

Usage:
  python make_songs_json.py --band3-recomp ../band3_recomp [dlc/songs.dta ...] --out ../songs.json
  python make_songs_json.py disc_songs.dta dlc1/songs.dta --out ../songs.json

Standard library only.
"""

import argparse
import importlib.util
import json
import os
import sys

# conditionals: a ship build defines _SHIP, and nothing else is assumed
SHIP_DEFINES = {'_SHIP'}


def tokens(text):
    """DTA's tokens: '(' ')' '{' '}' '[' ']', strings (str subclass Str), and
    bare words, directives (#ifndef and the like) included. Comments go."""
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if c.isspace():
            i += 1
        elif c == ';':
            while i < n and text[i] != '\n':
                i += 1
        elif text.startswith('/*', i):
            end = text.find('*/', i + 2)
            i = n if end < 0 else end + 2
        elif c in '(){}[]':
            yield c
            i += 1
        elif c == '"':
            j, out = i + 1, []
            while j < n and text[j] != '"':
                if text[j] == '\\' and j + 1 < n:
                    out.append({'q': '"', 'n': '\n', '\\': '\\', '"': '"'}.get(text[j + 1], text[j + 1]))
                    j += 2
                else:
                    out.append(text[j])
                    j += 1
            yield Str(''.join(out))
            i = j + 1
        elif c == "'":
            # a quoted symbol
            j = text.find("'", i + 1)
            j = n if j < 0 else j
            yield text[i + 1:j]
            i = j + 1
        else:
            j = i
            while j < n and not text[j].isspace() and text[j] not in '(){}[];"':
                j += 1
            yield text[i:j]
            i = j


class Str(str):
    """A quoted string, as against a symbol."""


def parse(text, defines=SHIP_DEFINES):
    """The top-level arrays of a DTA as nested lists, with #ifdef/#ifndef
    blocks kept or dropped by defines (which #define adds to)."""
    defines = set(defines)
    root = []
    stack = [root]
    # one entry per open conditional: whether its current branch is kept
    keep = []
    it = iter(tokens(text))
    for tok in it:
        if not isinstance(tok, Str) and tok.startswith('#'):
            if tok in ('#ifdef', '#ifndef'):
                name = next(it, '')
                on = (name in defines) == (tok == '#ifdef')
                keep.append(on)
            elif tok == '#else':
                if keep:
                    keep[-1] = not keep[-1]
            elif tok == '#endif':
                if keep:
                    keep.pop()
            elif tok == '#define':
                name = next(it, '')
                if all(keep):
                    defines.add(name)
            elif tok in ('#include', '#merge', '#undef', '#include_opt'):
                next(it, None)
            continue
        if not all(keep):
            # still track nesting, so a dropped block's arrays don't leak
            if tok in '([{' and len(tok) == 1:
                stack.append(None)
            elif tok in ')]}' and len(tok) == 1 and len(stack) > 1 and stack[-1] is None:
                stack.pop()
            continue
        if tok in ('(', '[', '{') and not isinstance(tok, Str):
            stack.append([])
        elif tok in (')', ']', '}') and not isinstance(tok, Str):
            if len(stack) > 1:
                done = stack.pop()
                if stack[-1] is not None and done is not None:
                    stack[-1].append(done)
        else:
            if stack[-1] is not None:
                stack[-1].append(tok)
    return root


def field(entry, key):
    """The first value of (key value ...) among an entry's own fields."""
    for item in entry[1:]:
        if isinstance(item, list) and item and item[0] == key and len(item) > 1:
            return item[1]
    return None


def songs(arrays):
    """{song_id: {name, artist}} for the real songs among top-level arrays."""
    out = {}
    for entry in arrays:
        if not entry or isinstance(entry[0], list):
            continue
        name, artist, song_id = field(entry, 'name'), field(entry, 'artist'), field(entry, 'song_id')
        if field(entry, 'fake') == 'TRUE' or not isinstance(name, Str) or not isinstance(artist, Str):
            continue
        try:
            song_id = int(song_id)
        except (TypeError, ValueError):
            # text song_ids are customs' (band3 and RB3Enhanced number them by CRC)
            continue
        if song_id > 0:
            out[song_id] = {'name': str(name), 'artist': str(artist)}
    return out


def read_text(path):
    with open(path, 'rb') as f:
        data = f.read()
    # DLC songs.dta is Latin-1 unless it says (encoding utf8); try UTF-8 first
    try:
        return data.decode('utf-8')
    except UnicodeDecodeError:
        return data.decode('latin-1')


def disc_dta(recomp):
    """The disc's songs.dtb as text, through band3_recomp's own archive reader."""
    tool = os.path.join(recomp, 'tools', 'read_game_config.py')
    spec = importlib.util.spec_from_file_location('read_game_config', tool)
    rgc = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(rgc)
    ark = rgc.Ark(gen=os.path.join(recomp, 'assets', 'gen'))
    return rgc.DtbPrinter(rgc.decrypt(ark.read('songs/gen/songs.dtb'))).print()


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.split('\n\n')[0])
    ap.add_argument('dta', nargs='*', help='text songs.dta files, later ones winning')
    ap.add_argument('--band3-recomp', metavar='PATH', help="read the disc's songs from this band3_recomp checkout")
    ap.add_argument('--out', default=os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'songs.json'),
                    help='where to write (default: site/songs.json)')
    args = ap.parse_args(argv)
    if not args.dta and not args.band3_recomp:
        ap.error('give at least one songs.dta, or --band3-recomp')

    sources = []
    if args.band3_recomp:
        sources.append(('disc (' + args.band3_recomp + ')', disc_dta(args.band3_recomp)))
    for path in args.dta:
        sources.append((path, read_text(path)))
    found = {}
    for label, text in sources:
        these = songs(parse(text))
        print(f'{label}: {len(these)} songs', file=sys.stderr)
        found.update(these)

    body = {str(k): found[k] for k in sorted(found)}
    with open(args.out, 'w', encoding='utf-8', newline='\n') as f:
        # one song a line, for readable diffs
        lines = [f'{json.dumps(k)}: {json.dumps(v, ensure_ascii=False)}' for k, v in body.items()]
        f.write('{\n' + ',\n'.join(lines) + '\n}\n')
    print(f'{os.path.normpath(args.out)}: {len(body)} songs', file=sys.stderr)


if __name__ == '__main__':
    main()
