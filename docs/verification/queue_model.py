#!/usr/bin/env python3
"""Bounded protocol model for pgt::mpsc SpscRing and MpscRing.

This is an executable falsifier, not an unbounded proof.  It explores a small
sequentially-consistent model of the queue protocols, with release/acquire
publication edges represented as explicit transition ordering constraints.
"""

from __future__ import annotations

import argparse
import dataclasses
import sys
from collections import deque
from typing import Callable, Iterable, Literal


RING = 4
WRITERS = (1, 2)
EXTENTS = (1, 2)
MAX_SPSC_DEPTH = 9
MAX_MPSC_DEPTH = 11


class ModelViolation(AssertionError):
  def __init__(self, variant: str, mutant: str, trace: tuple[str, ...], message: str):
    super().__init__(f"{variant}:{mutant}: {message}\ntrace:\n  " + "\n  ".join(trace))
    self.variant = variant
    self.mutant = mutant
    self.trace = trace
    self.message = message


@dataclasses.dataclass(frozen=True, order=True)
class SSlot:
  extent: int = 0
  payload: int = -1


@dataclasses.dataclass(frozen=True, order=True)
class SRec:
  pos: int
  extent: int
  payload: int
  published: bool
  popped: bool = False


@dataclasses.dataclass(frozen=True, order=True)
class SpscState:
  slots: tuple[SSlot, ...] = (SSlot(), SSlot(), SSlot(), SSlot())
  publish: int = 0
  read: int = 0
  tail: int = 0
  owner: Literal["free", "live", "stopped", "dead"] = "live"
  gen: int = 1
  writer_gen: int = 1
  pc: Literal["idle", "reserved", "payload", "slot", "detached"] = "idle"
  res_pos: int = -1
  res_extent: int = 0
  res_payload: int = -1
  read_cache: int = 0
  records: tuple[SRec, ...] = ()
  delivered: tuple[int, ...] = ()
  peek_pos: int = -1
  next_payload: int = 1
  stale_owner: bool = False


def replace(obj, **kwargs):
  return dataclasses.replace(obj, **kwargs)


def _span(pos: int, extent: int) -> set[int]:
  return {(pos + i) % RING for i in range(extent)}


def _published_live_spsc(s: SpscState) -> list[SRec]:
  return [r for r in s.records if r.published and not r.popped]


def _with_srec(s: SpscState, payload: int, fn: Callable[[SRec], SRec]) -> tuple[SRec, ...]:
  return tuple(fn(r) if r.payload == payload else r for r in s.records)


def spsc_invariants(s: SpscState, mutant: str, trace: tuple[str, ...]) -> None:
  if not (s.read <= s.publish <= s.tail <= s.read + RING):
    raise ModelViolation("spsc", mutant, trace, "cursor order or capacity window broke")
  if s.peek_pos != -1 and s.peek_pos < s.read:
    raise ModelViolation("spsc", mutant, trace, "peek points behind read cursor")

  p = s.read
  seen_payloads: list[int] = []
  while p < s.publish:
    slot = s.slots[p % RING]
    if slot.extent <= 0:
      raise ModelViolation("spsc", mutant, trace, "published tail exposes a missing length slot")
    seen_payloads.append(slot.payload)
    p += slot.extent
    if p > s.publish:
      raise ModelViolation("spsc", mutant, trace, "slot chain overshoots publication tail")
  if p != s.publish:
    raise ModelViolation("spsc", mutant, trace, "slot chain does not reach publication tail")

  published_payloads = [r.payload for r in s.records if r.published and not r.popped]
  if seen_payloads != published_payloads:
    raise ModelViolation("spsc", mutant, trace, "published slot chain diverges from committed records")
  if len(set(s.delivered)) != len(s.delivered):
    raise ModelViolation("spsc", mutant, trace, "duplicate retired delivery")
  if s.delivered != tuple(r.payload for r in s.records if r.popped):
    raise ModelViolation("spsc", mutant, trace, "retired delivery order diverges from popped records")

  used: dict[int, str] = {}
  for rec in _published_live_spsc(s):
    for cell in _span(rec.pos, rec.extent):
      owner = used.setdefault(cell, f"published:{rec.payload}")
      if owner != f"published:{rec.payload}":
        raise ModelViolation("spsc", mutant, trace, "published records overlap physically")
  if s.pc in {"reserved", "payload", "slot"}:
    for cell in _span(s.res_pos, s.res_extent):
      if cell in used:
        raise ModelViolation("spsc", mutant, trace, "private reservation overlaps live published record")
      used[cell] = f"reservation:{s.res_payload}"

  if s.owner == "live" and s.writer_gen != s.gen:
    raise ModelViolation("spsc", mutant, trace, "live writer bound to stale slot generation")
  if s.stale_owner:
    raise ModelViolation("spsc", mutant, trace, "stale displaced writer can still pass ownership")


def spsc_transitions(s: SpscState, mutant: str) -> Iterable[tuple[str, SpscState]]:
  owner_can_write = s.owner == "live" and s.writer_gen == s.gen
  if owner_can_write and s.pc == "idle":
    for extent in EXTENTS:
      if mutant == "no_capacity" or s.tail + extent - s.read_cache <= RING:
        yield f"reserve({extent})", replace(
          s,
          pc="reserved",
          res_pos=s.tail,
          res_extent=extent,
          res_payload=s.next_payload,
          next_payload=s.next_payload + 1,
        )
    if s.tail + 1 - s.read_cache > RING:
      yield "refresh_read_cache", replace(s, read_cache=s.read)
  if owner_can_write and s.pc == "reserved":
    if mutant == "publish_before_desc":
      rec = SRec(s.res_pos, s.res_extent, s.res_payload, True)
      yield "MUTANT_publish_before_desc", replace(
        s,
        publish=s.res_pos + s.res_extent,
        tail=s.res_pos + s.res_extent,
        pc="idle",
        records=s.records + (rec,),
        res_pos=-1,
        res_extent=0,
        res_payload=-1,
      )
    yield "payload_store", replace(s, pc="payload")
    yield "abort_private", replace(s, pc="idle", res_pos=-1, res_extent=0, res_payload=-1)
  if owner_can_write and s.pc == "payload":
    slots = list(s.slots)
    slots[s.res_pos % RING] = SSlot(s.res_extent, s.res_payload)
    yield "slot_store", replace(s, slots=tuple(slots), pc="slot")
  if owner_can_write and s.pc == "slot":
    rec = SRec(s.res_pos, s.res_extent, s.res_payload, True)
    yield "publish_tail", replace(
      s,
      publish=s.res_pos + s.res_extent,
      tail=s.res_pos + s.res_extent,
      pc="idle",
      records=s.records + (rec,),
      res_pos=-1,
      res_extent=0,
      res_payload=-1,
    )
  if s.peek_pos == -1 and s.read < s.publish:
    slot = s.slots[s.read % RING]
    if slot.extent > 0:
      yield "reader_peek", replace(s, peek_pos=s.read)
  if s.peek_pos != -1:
    slot = s.slots[s.peek_pos % RING]

    def pop_rec(r: SRec) -> SRec:
      return replace(r, popped=True) if r.pos == s.peek_pos else r

    yield "reader_pop", replace(
      s,
      read=s.peek_pos + slot.extent,
      peek_pos=-1,
      delivered=s.delivered + (slot.payload,),
      records=tuple(pop_rec(r) for r in s.records),
    )
  if s.owner == "live":
    yield "writer_stop", replace(s, owner="stopped")
    yield "writer_die", replace(s, owner="dead")
    if s.pc == "idle":
      yield "detach_writer", replace(s, owner="free", pc="detached", writer_gen=0)
  if s.owner == "free":
    yield "attach_free", replace(s, owner="live", pc="idle", writer_gen=s.gen, read_cache=s.read)
  if s.owner == "dead" and s.read == s.publish:
    next_gen = s.gen if mutant == "no_owner_generation" else s.gen + 1
    yield "takeover_dead_drained", replace(
      s,
      owner="live",
      gen=next_gen,
      writer_gen=next_gen,
      pc="idle",
      read_cache=s.read,
      stale_owner=(mutant == "no_owner_generation"),
    )
  if mutant == "takeover_live" and s.owner in {"live", "stopped"}:
    yield "MUTANT_takeover_live", replace(
      s, owner="live", gen=s.gen + 1, writer_gen=s.gen + 1, stale_owner=True
    )


@dataclasses.dataclass(frozen=True, order=True)
class Claim:
  state: Literal["FREE", "CLAIMED", "CLEARED", "ABORTED"] = "FREE"
  pos: int = 0
  extent: int = 0
  owner: int = 0


@dataclasses.dataclass(frozen=True, order=True)
class Result:
  tag: int = -1
  payload: int = -1
  complete: bool = False


@dataclasses.dataclass(frozen=True, order=True)
class Writer:
  alive: bool = True
  pc: Literal["idle", "claimed", "promoted", "vouched", "payload", "done"] = "idle"
  p: int = -1
  need: int = 0
  payload: int = -1


@dataclasses.dataclass(frozen=True, order=True)
class MRec:
  pos: int
  extent: int
  writer: int
  payload: int
  committed: bool = False
  aborted: bool = False
  popped: bool = False


@dataclasses.dataclass(frozen=True, order=True)
class MpscState:
  claim: tuple[Claim, ...] = (
    Claim("FREE", 0, 0, 0),
    Claim("FREE", -100, 0, 0),
    Claim("FREE", -100, 0, 0),
    Claim("FREE", -100, 0, 0),
  )
  result: tuple[Result, ...] = (Result(), Result(), Result(), Result())
  hint: int = 0
  read: int = 0
  frontier: int = 0
  writers: tuple[Writer, ...] = (Writer(), Writer())
  records: tuple[MRec, ...] = ()
  delivered: tuple[int, ...] = ()
  peek_pos: int = -1
  next_payload: int = 1
  bad_claim: bool = False


def _cell(pos: int) -> int:
  return pos % RING


def _claim_at(s: MpscState, pos: int) -> Claim:
  return s.claim[_cell(pos)]


def _set_claim(s: MpscState, pos: int, claim: Claim) -> tuple[Claim, ...]:
  cells = list(s.claim)
  cells[_cell(pos)] = claim
  return tuple(cells)


def _set_result(s: MpscState, pos: int, result: Result) -> tuple[Result, ...]:
  cells = list(s.result)
  cells[_cell(pos)] = result
  return tuple(cells)


def _set_writer(s: MpscState, wid: int, writer: Writer) -> tuple[Writer, ...]:
  writers = list(s.writers)
  writers[wid - 1] = writer
  return tuple(writers)


def _with_mrec(s: MpscState, payload: int, fn: Callable[[MRec], MRec]) -> tuple[MRec, ...]:
  return tuple(fn(r) if r.payload == payload else r for r in s.records)


def mpsc_walk_frontier(s: MpscState, mutant: str) -> int | None:
  p = s.hint
  prev_claimed = False
  for _ in range(RING + 1):
    w = _claim_at(s, p)
    free_matches = w.state == "FREE" and (mutant == "free_state_only" or w.pos == p)
    if free_matches:
      if prev_claimed and mutant not in {"allow_successor_after_claimed", "early_hint"}:
        return None
      return p
    if w.state == "FREE":
      return None
    if w.extent <= 0:
      return None
    prev_claimed = w.state == "CLAIMED"
    p += w.extent
  return None


def _active_mpsc_records(s: MpscState) -> list[MRec]:
  return [r for r in s.records if not r.popped and not r.aborted]


def mpsc_invariants(s: MpscState, mutant: str, trace: tuple[str, ...]) -> None:
  if s.bad_claim:
    raise ModelViolation("mpsc", mutant, trace, "writer claimed a non-self-certifying FREE word")
  if s.hint > s.frontier:
    raise ModelViolation("mpsc", mutant, trace, "hint leads ghost frontier")
  used: dict[int, str] = {}
  for rec in _active_mpsc_records(s):
    if rec.pos > s.frontier:
      raise ModelViolation("mpsc", mutant, trace, "record was claimed beyond the ghost frontier")
    for cell in _span(rec.pos, rec.extent):
      owner = used.setdefault(cell, f"record:{rec.payload}")
      if owner != f"record:{rec.payload}":
        raise ModelViolation("mpsc", mutant, trace, "live records overlap physically")

  for rec in s.records:
    c = _claim_at(s, rec.pos)
    if c.pos == rec.pos and c.state in {"CLEARED", "ABORTED"}:
      q = rec.pos + rec.extent
      succ = _claim_at(s, q)
      successor_claimed = any(other.pos == q for other in s.records)
      if not successor_claimed and (succ.state != "FREE" or succ.pos != q):
        raise ModelViolation("mpsc", mutant, trace, "vouched or aborted record lacks promoted successor")
    if c.pos == rec.pos and c.state == "CLAIMED":
      q = rec.pos + rec.extent
      for other in s.records:
        if other.pos == q and not other.aborted:
          raise ModelViolation("mpsc", mutant, trace, "successor of CLAIMED record was claimed")

  exact_free = [i for i, c in enumerate(s.claim) if c.state == "FREE" and c.pos == s.frontier]
  if len(exact_free) > 1:
    raise ModelViolation("mpsc", mutant, trace, "more than one exact frontier FREE word")

  committed = [r for r in sorted(s.records, key=lambda r: r.pos) if r.committed and not r.aborted]
  delivered_records = [r for r in sorted(s.records, key=lambda r: r.pos) if r.popped]
  if len(set(s.delivered)) != len(s.delivered):
    raise ModelViolation("mpsc", mutant, trace, "duplicate delivery")
  if tuple(r.payload for r in delivered_records) != s.delivered:
    raise ModelViolation("mpsc", mutant, trace, "delivery order does not match reader pops")
  if list(s.delivered) != [r.payload for r in committed[: len(s.delivered)]]:
    raise ModelViolation("mpsc", mutant, trace, "delivered sequence is not committed FIFO prefix")

  for i, res in enumerate(s.result):
    if res.tag == -1:
      continue
    recs = [r for r in s.records if r.pos == res.tag]
    if not recs or not recs[0].committed or recs[0].aborted or not res.complete:
      raise ModelViolation("mpsc", mutant, trace, "result tag does not certify a complete current record")
    if _cell(res.tag) != i:
      raise ModelViolation("mpsc", mutant, trace, "result tag stored in wrong cell")


def mpsc_transitions(s: MpscState, mutant: str) -> Iterable[tuple[str, MpscState]]:
  for wid in WRITERS:
    w = s.writers[wid - 1]
    if w.alive and w.pc == "idle":
      p = mpsc_walk_frontier(s, mutant)
      if p is not None:
        for need in EXTENTS:
          cap_ok = p + need + (0 if mutant == "no_slack" else 1) - s.read <= RING
          if cap_ok:
            claim = Claim("CLAIMED", p, need, wid)
            rec = MRec(p, need, wid, s.next_payload)
            yield f"w{wid}_claim({need})", replace(
              s,
              claim=_set_claim(s, p, claim),
              writers=_set_writer(s, wid, Writer(True, "claimed", p, need, s.next_payload)),
              records=s.records + (rec,),
              next_payload=s.next_payload + 1,
              bad_claim=s.bad_claim or (mutant == "free_state_only" and _claim_at(s, p).pos != p),
            )
    if w.alive and w.pc == "claimed":
      if mutant != "recover_no_promote":
        q = w.p + w.need
        yield f"w{wid}_promote", replace(
          s,
          claim=_set_claim(s, q, Claim("FREE", q, 0, 0)),
          writers=_set_writer(s, wid, replace(w, pc="promoted")),
          frontier=max(s.frontier, q),
        )
      yield f"w{wid}_die_before_promote", replace(
        s, writers=_set_writer(s, wid, replace(w, alive=False))
      )
    if w.alive and w.pc == "promoted":
      if mutant == "early_hint":
        yield f"MUTANT_w{wid}_early_hint", replace(s, hint=w.p + w.need)
      yield f"w{wid}_vouch", replace(
        s,
        claim=_set_claim(s, w.p, Claim("CLEARED", w.p, w.need, wid)),
        hint=w.p + w.need,
        writers=_set_writer(s, wid, replace(w, pc="vouched")),
      )
      yield f"w{wid}_die_after_promote", replace(
        s, writers=_set_writer(s, wid, replace(w, alive=False))
      )
    if w.alive and w.pc == "vouched":
      if mutant == "tag_before_payload":
        yield f"MUTANT_w{wid}_tag_before_payload", replace(
          s, result=_set_result(s, w.p, Result(w.p, w.payload, False))
        )
      yield f"w{wid}_payload", replace(s, writers=_set_writer(s, wid, replace(w, pc="payload")))
      yield f"w{wid}_abort", replace(
        s,
        claim=_set_claim(s, w.p, Claim("ABORTED", w.p, w.need, wid)),
        records=_with_mrec(s, w.payload, lambda r: replace(r, aborted=True)),
        writers=_set_writer(s, wid, Writer()),
      )
      yield f"w{wid}_die_after_vouch", replace(
        s, writers=_set_writer(s, wid, replace(w, alive=False))
      )
    if w.alive and w.pc == "payload":
      tag = 1 if mutant == "boolean_tag" else w.p
      result = Result(tag, w.payload, True)
      yield f"w{wid}_commit", replace(
        s,
        result=_set_result(s, w.p, result),
        records=_with_mrec(s, w.payload, lambda r: replace(r, committed=True)),
        writers=_set_writer(s, wid, Writer(True, "idle")),
      )
      yield f"w{wid}_die_before_commit", replace(
        s, writers=_set_writer(s, wid, replace(w, alive=False))
      )

  if s.peek_pos == -1:
    c = _claim_at(s, s.read)
    if c.state == "ABORTED" and c.pos == s.read and c.extent > 0:
      yield "reader_skip_aborted", replace(s, read=s.read + c.extent)
    elif c.state in {"CLAIMED", "CLEARED"} and c.pos == s.read:
      res = s.result[_cell(s.read)]
      tag_committed = res.tag == s.read and res.complete
      if tag_committed and mutant != "recover_no_tag_recheck":
        yield "reader_peek_committed", replace(s, peek_pos=s.read)
      else:
        owner = s.writers[c.owner - 1]
        if not owner.alive:
          if mutant == "recover_no_tag_recheck" or not tag_committed:
            claim = s.claim
            frontier = s.frontier
            if c.state == "CLAIMED" and mutant != "recover_no_promote":
              claim = _set_claim(s, s.read + c.extent, Claim("FREE", s.read + c.extent, 0, 0))
              frontier = max(frontier, s.read + c.extent)
            claim = tuple(
              Claim("ABORTED", s.read, c.extent, c.owner) if i == _cell(s.read) else x
              for i, x in enumerate(claim)
            )
            yield "reader_recover_dead", replace(
              s,
              claim=claim,
              frontier=frontier,
              records=_with_mrec(s, next((r.payload for r in s.records if r.pos == s.read), -1),
                                  lambda r: replace(r, aborted=True)),
            )
  if s.peek_pos != -1:
    c = _claim_at(s, s.peek_pos)
    rec = next(r for r in s.records if r.pos == s.peek_pos)
    yield "reader_pop", replace(
      s,
      read=s.peek_pos + c.extent,
      peek_pos=-1,
      delivered=s.delivered + (rec.payload,),
      records=_with_mrec(s, rec.payload, lambda r: replace(r, popped=True)),
    )


def explore(
  variant: Literal["spsc", "mpsc"],
  mutant: str = "base",
  max_depth: int | None = None,
) -> dict[str, int]:
  if variant == "spsc":
    init = SpscState()
    transitions = spsc_transitions
    invariants = spsc_invariants
    depth = MAX_SPSC_DEPTH if max_depth is None else max_depth
  else:
    init = MpscState()
    transitions = mpsc_transitions
    invariants = mpsc_invariants
    depth = MAX_MPSC_DEPTH if max_depth is None else max_depth

  q = deque([(init, ())])
  seen = {init}
  edges = 0
  while q:
    state, trace = q.popleft()
    invariants(state, mutant, trace)
    if len(trace) >= depth:
      continue
    for label, nxt in sorted(transitions(state, mutant), key=lambda item: item[0]):
      ntrace = trace + (label,)
      invariants(nxt, mutant, ntrace)
      edges += 1
      if nxt not in seen:
        seen.add(nxt)
        q.append((nxt, ntrace))
  return {"states": len(seen), "edges": edges, "depth": depth}


def bounded_progress() -> dict[str, bool]:
  spsc_dead_drained = SpscState(owner="dead")
  spsc_stopped = SpscState(owner="stopped")
  if not any(label == "takeover_dead_drained" for label, _ in spsc_transitions(spsc_dead_drained, "base")):
    raise ModelViolation("spsc", "base", (), "dead drained writer has no takeover transition")
  if any("takeover" in label for label, _ in spsc_transitions(spsc_stopped, "base")):
    raise ModelViolation("spsc", "base", (), "stopped writer can be taken over")

  # Dead untagged MPSC head must recover, then be skipped by the reader.
  s = MpscState()
  trace: tuple[str, ...] = ()
  for want in ("w1_claim(1)", "w1_die_before_promote", "reader_recover_dead", "reader_skip_aborted"):
    choices = dict(mpsc_transitions(s, "base"))
    if want not in choices:
      raise ModelViolation("mpsc", "base", trace, f"progress transition {want} unavailable")
    s = choices[want]
    trace += (want,)
    mpsc_invariants(s, "base", trace)
  return {"spsc_dead_takeover": True, "mpsc_dead_recovery": True}


def seeded_mutant_violation(variant: str, mutant: str) -> None:
  if variant == "mpsc" and mutant == "free_state_only":
    seed = MpscState(hint=1, read=0, frontier=1)
    choices = dict(mpsc_transitions(seed, mutant))
    nxt = choices["w1_claim(1)"]
    mpsc_invariants(nxt, mutant, ("seed_wrong_lap_free", "w1_claim(1)"))
  if variant == "mpsc" and mutant == "recover_no_tag_recheck":
    s = MpscState()
    trace = []
    for step in ("w1_claim(1)", "w1_promote", "w1_vouch", "w1_payload", "w1_commit"):
      s = dict(mpsc_transitions(s, "base"))[step]
      trace.append(step)
    s = replace(s, writers=_set_writer(s, 1, replace(s.writers[0], alive=False)))
    choices = dict(mpsc_transitions(s, mutant))
    nxt = choices["reader_recover_dead"]
    mpsc_invariants(nxt, mutant, tuple(trace + ["w1_dies_after_commit", "reader_recover_dead"]))
  if variant == "mpsc" and mutant == "recover_no_promote":
    s = MpscState()
    trace = ("w1_claim(1)", "w1_die_before_promote", "reader_recover_dead")
    for step in trace:
      s = dict(mpsc_transitions(s, mutant))[step]
    # The head is now aborted but the successor never became this-lap public.
    mpsc_invariants(s, mutant, trace)


SPSC_MUTANTS = ("publish_before_desc", "no_owner_generation", "takeover_live", "no_capacity")
MPSC_MUTANTS = (
  "free_state_only",
  "allow_successor_after_claimed",
  "early_hint",
  "recover_no_tag_recheck",
  "recover_no_promote",
  "boolean_tag",
  "no_slack",
  "tag_before_payload",
)


def run_suite(variant: str, include_mutants: bool) -> dict[str, dict[str, int] | dict[str, str]]:
  variants = ("spsc", "mpsc") if variant == "all" else (variant,)
  results: dict[str, dict[str, int] | dict[str, str]] = {}
  for v in variants:
    results[f"{v}:base"] = explore(v)  # raises on failure
  if "spsc" in variants:
    bounded_progress()
  if include_mutants:
    for v in variants:
      mutants = SPSC_MUTANTS if v == "spsc" else MPSC_MUTANTS
      for mutant in mutants:
        try:
          explore(v, mutant)
        except ModelViolation as exc:
          results[f"{v}:{mutant}"] = {
            "rejected": exc.message,
            "trace": " / ".join(exc.trace),
          }
        else:
          try:
            seeded_mutant_violation(v, mutant)
          except ModelViolation as exc:
            results[f"{v}:{mutant}"] = {
              "rejected": exc.message,
              "trace": " / ".join(exc.trace),
            }
          else:
            raise ModelViolation(v, mutant, (), "negative-control mutant survived")
  return results


def main(argv: list[str] | None = None) -> int:
  parser = argparse.ArgumentParser()
  parser.add_argument("--variant", choices=("spsc", "mpsc", "all"), default="all")
  parser.add_argument("--mutants", action="store_true")
  args = parser.parse_args(argv)
  results = run_suite(args.variant, args.mutants)
  for name in sorted(results):
    print(f"{name}: {results[name]}")
  return 0


if __name__ == "__main__":
  try:
    raise SystemExit(main())
  except ModelViolation as exc:
    print(exc, file=sys.stderr)
    raise SystemExit(1)
