# csc snapshot v2 — shared digest fixture

`snapshot_digest_v2.json` is the cross-implementation contract for the snapshot
digest. The Go server tests against it (`internal/syncsnapshot`); csc must ship a
test that asserts the same file with its own implementation.

It exists because the digest is the gate on deletion. A client may remove a
locally managed capability only after it has reassembled a whole snapshot and
recomputed `snapshotDigest`. If the two implementations disagree by one byte,
the client discards every snapshot forever and simply stops converging — no
error, no server-side symptom, and the divergence is only visible as "this
device never applies anything". "We both implemented RFC 8785" is not evidence
that they agree; this file is.

## What to reproduce

1. **`expected.canonical`** — the exact canonical serialization, byte for byte.
2. **`expected.snapshotDigest`** — lowercase SHA-256 hex of those UTF-8 bytes.
3. **`expected.pages`** — the deterministic page slicing at `pageSize`, with the
   element text each page carries.

`expected.contentDigest` is a server-internal change-detection key (it decides
whether a new generation is allocated). csc never sees it; it is pinned here so
the rule cannot be changed by accident.

## The document being hashed

Reassemble the snapshot from its pages: concatenate every page's `items` in page
order, then every page's `tombstones` in page order. Then serialize

```
{contractVersion, snapshotId, generation, generatedAt,
 pageCount, itemCount, tombstoneCount, items, tombstones}
```

with RFC 8785 (JSON Canonicalization Scheme) and take the lowercase SHA-256 hex
of the UTF-8 bytes.

`pageIndex`, `complete` and `snapshotDigest` are excluded by construction: the
first two are page-local and the third is the output.

Element order is part of the contract and is fixed by the server: items by
`itemId`, tombstones by `(itemId, eventId)`. Do not re-sort, and do not rely on
arrival order for anything else.

## Canonicalization rules that actually bite

The fixture's strings are chosen to hit each of these, because each is a place a
mainstream JSON encoder deviates from JCS:

| Rule | Why it is in the fixture |
| --- | --- |
| `<`, `>`, `&` are emitted literally | Go's `encoding/json` and several JS helpers HTML-escape them by default. A capability name may contain any of them. |
| U+2028 / U+2029 are emitted literally | Go's `encoding/json` escapes them *unconditionally*, even with HTML escaping disabled. `JSON.stringify` does not. |
| Short escapes only for `"` `\` `\b` `\f` `\n` `\r` `\t` | Everything else below U+0020 uses `\u00xx` with **lowercase** hex. |
| Everything at or above U+0020 is literal UTF-8 | Including CJK, emoji, and supplementary-plane characters such as U+1D11E. |
| Object keys sorted by **UTF-16 code unit** | Not by UTF-8 byte order. They agree below U+10000 and disagree above it. |
| No insignificant whitespace | |
| Integers only | This contract forbids fractional numbers outright, so nobody has to implement ECMAScript `Number::toString`. Every number here is a count, a generation or an index, and integers within ±2^53 have exactly one representation: plain decimal. Reject anything else rather than guessing. |

## Transport is not the contract

Do **not** hash the raw bytes you received. Parse the elements and
re-canonicalize them. A JSON transport is free to re-escape `<` or U+2028 on the
way through, and an intermediary may do so even where this server does not
(`c.PureJSON` plus a canonical-bytes `MarshalJSON` keep the response identical to
the hashed form, but that is a property of this server, not of HTTP).

Parse numbers as integers, not floats: `9007199254740991` must survive the round
trip. A float64 decode silently rounds it and produces a well-formed digest that
matches nothing.

## Regenerating

```
cd server && SYNCSNAPSHOT_UPDATE_FIXTURE=1 go test ./internal/syncsnapshot/ -run TestDigestFixture_Regenerate
```

The input half lives in `fixture_gen_test.go` (`fixtureSource`); the expected
half is always derived from it. Regeneration is opt-in so an unintended change
to the canonical shape fails `TestDigestFixture_MatchesEmbedded` instead of
quietly rewriting the file csc also depends on. **Regenerating is a contract
change**: it must land together with the csc side.
