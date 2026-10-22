Offline eMudhra regression fixtures
==================================

Downloaded on 2026-10-22:

  chains/*.pem:
    https://emudhra.we-have-a-problem.com/chains.zip
    Archive SHA-256:
    7b949956955fc501a30f4d98d2f513d34c0bbbe92e29a968194c96e097ce2345
    The upstream explanation is preserved in chains/README.txt.

  affected-roots.pem:
    The four public certificates from
    https://emudhra.we-have-a-problem.com/roots.json

  isrg-root-x1.pem:
    https://letsencrypt.org/certs/isrgrootx1.pem
    An unaffected, widely trusted root used as a positive bundle control.

These are public certificates only; no private keys are included.
The tests use a fixed verification time, 2026-10-22T16:00:00Z, so expiration
cannot turn a vulnerable verifier into an apparent success.

The fourteen chains cover all six affected TLS issuing keys, their Sectigo
cross-signs, and both non-CT test chains. The incident's two code-signing-only
intermediates are not TLS paths. Go 1.27.3 distrusts all eight compromised keys.

The Go checks deliberately trust the affected roots to exercise Go's key
blocklist independently of the host OS. The bundle check separately rejects
the four affected root keys and six TLS intermediate keys, rather than relying
on patched Go to hide stale trust anchors from non-Go consumers.
