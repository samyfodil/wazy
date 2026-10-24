#!/usr/bin/env python3
"""Writes mutations.txt: each seed (mutations/*.wit) cut short, and with a
character no WIT token can hold ('$') put in, at the start of each of its
tokens -- every place the parser can find the end of a file or a lexer
error -- as "name|base64(source)" lines, for gen-probes.sh to run through
wit-parser."""
import base64, os, re

here = os.path.dirname(os.path.abspath(__file__))
with open(os.path.join(here, "mutations.txt"), "w") as out:
    for seedname in sorted(os.listdir(os.path.join(here, "mutations"))):
        seed = open(os.path.join(here, "mutations", seedname), encoding="utf-8").read()
        tag = seedname[: -len(".wit")]
        starts = sorted({m.start() for m in re.finditer(r"[A-Za-z0-9_%-]+|\s+|.", seed, re.S)})
        for k in starts:
            for name, src in (("%s-eof-%d" % (tag, k), seed[:k]), ("%s-bad-%d" % (tag, k), seed[:k] + "$" + seed[k:])):
                out.write("%s|%s\n" % (name, base64.b64encode(src.encode()).decode()))
