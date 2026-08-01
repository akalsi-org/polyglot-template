# Bounded SPSC/MPSC Queue Model

`queue_model.py` is an executable, dependency-free Python model for the current
`SpscRing<>` and `MpscRing<>` protocols. It is a bounded falsifier, not an
unbounded proof of the C++ implementation or of every weak-memory execution.

Run it with:

```bash
./repo.sh python docs/verification/queue_model.py --variant all --mutants
./repo.sh python test/test_queue_model.py
```

## Bounds

- Ring size: 4 abstract grains.
- Record extents: 1 or 2 grains.
- MPSC writers: 2.
- SPSC search depth: 9 protocol transitions.
- MPSC search depth: 11 protocol transitions.
- Positions do not model `uint64_t` wraparound.
- Release/acquire visibility is represented by explicit transition ordering
  constraints such as slot-before-tail and payload-before-tag.

## SPSC Coverage

The SPSC model covers private reservation, payload fill, length-slot store,
tail publication, reader peek/pop, voluntary detach, death, stopped owners,
dead-writer takeover only after drain, slot generation fencing, and capacity
overwrite protection.

The model is anchored to these implementation facts:

- `SpscRing::reserve()` reserves privately after a capacity check.
- `SpscRing::commit()` stores the payload length slot before the release-store
  publication tail.
- `SpscRing::abort()` keeps abort writer-local.
- `SpscRing::peek()` and `SpscRing::pop()` acquire the tail, consume committed
  length slots, and release `read_pos`.
- `SpscRing::attachWriter()` gates dead-writer takeover on proven death plus a
  drained ring.
- `SpscRing::ownsWriter()` requires tid, slot owner, and slot generation.

Rejected SPSC negative controls: `publish_before_desc`, `no_owner_generation`,
`takeover_live`, and `no_capacity`.

## MPSC Coverage

The MPSC model covers exact `FREE(p)` claims, wrong-lap `FREE` rejection,
successor promotion, vouch publication, hint lag/lead constraints, exact commit
tags, abort, reader delivery, dead-writer recovery, successor-while-claimed
rejection, and the admission slack that keeps the successor grain unrecycled.

The model is anchored to these implementation facts:

- `MpscRing::reserve()` and `MpscRing::reserveSlow()` load the hint and claim
  only exact `freeWord(p)`, reject wrong-lap free words, block a `kClaimed`
  record's successor, and enforce admission slack.
- `MpscRing::finishClaim()` promotes the successor before the release vouch.
- `MpscRing::commit()` publishes payload with an exact position tag while
  leaving Claim as the full reserved extent.
- `MpscRing::peek()` prefers exact committed tags and rechecks the tag before
  recovery.
- `MpscRing::recover()` handles only a proven-dead writer's untagged record and
  promotes the successor for a still-`CLAIMED` record.

Rejected MPSC negative controls: `free_state_only`,
`allow_successor_after_claimed`, `early_hint`, `recover_no_tag_recheck`,
`recover_no_promote`, `boolean_tag`, `no_slack`, and `tag_before_payload`.

## Gaps

This model does not prove ISO C++ shared-memory object lifetime, lock-free
cross-process atomicity, PID namespace assumptions, `/proc` death-oracle
soundness, full AArch64 weak-memory behavior, scheduler fairness, or unbounded
progress. It supports the narrower claim that the modeled protocol transitions
preserve the listed safety invariants inside the declared bounds, and that the
negative controls are rejected by the same invariants.
