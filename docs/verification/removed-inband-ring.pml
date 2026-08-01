/*
 * MODELS A REMOVED IMPLEMENTATION.
 *
 * This is the Promela model of the removed in-band queue, superseded by
 * MpscRing. It is retained because the invariants
 * and the negative controls are reusable, NOT because it verifies anything that
 * currently ships.
 *
 * It is not a model of the two-plane protocol: there the successor slot is a
 * Claim cell rather than in-band payload, which is precisely what makes walking
 * past an in-flight record legal, so the walker rules modelled here do not
 * apply. Do not cite this file as evidence about the shipping queue.
 */

/*
 * Spin model of the variable-length MPSC byte queue (mpsc-algorithm.md, r3).
 *
 * Abstraction: 64-byte units -> "slots". in-band queue has R slots. Positions are
 * unwrapped monotonic; physical slot = pos % R, so pos and pos+R alias the
 * SAME descriptor word (the lap-ABA source).
 *
 * Descriptor word = {dstate, dfpos, dsize, dtid}. All real stores are full
 * 64-bit word stores, so every model store writes all four fields; CAS
 * compares all four fields (word equality).
 *
 * Ghost state (not part of any word / CAS):
 *   ddpos[slot]   unwrapped position of the record instance last claimed here
 *   dgcom[slot]   this instance was actually committed (S3)
 *   covered[slot] slot is inside a live (claimed, not yet popped) record (S1/S2)
 *   boundary[pos] pos is a real record boundary (S4)
 *   g_frontier    true next-unclaimed position (Lemma 1 check)
 *
 * Crash model: Reaper may set alive[w]=0 at any time, at most once per
 * writer. Every writer step is guarded "alive -> step" with an always-
 * enabled "!alive -> goto wdead" escape, so death is possible at EVERY
 * step boundary. Stalls are ordinary interleaving. Reader never dies.
 *
 * Fault injections (each re-introduces an r1 defect):
 *   -DBUG1  FREE is a plain zero-state predicate; claim CAS and walk do not
 *           compare the FREE word's position  -> lap ABA
 *   -DBUG2  recover() stores ABORTED without stamping the successor -> I2
 *   -DBUG3  writers help: a writer finding a dead CLAIMED owner performs
 *           recovery itself -> delayed-stamper race (needs NW=3)
 *   -DFIXSTALE  proposed fix for the stale-descriptor recovery defect the
 *           base model finds: recover re-reads the descriptor after the
 *           death check instead of acting on the peek-time snapshot.
 */

#ifndef NW
#define NW 2
#endif
#ifndef NOPS
#define NOPS 3
#endif
#ifndef R
#define R 4
#endif
#define MAXPOS 48

#define D_FREE      0
#define D_CLAIMED   1
#define D_CLEARED   2
#define D_COMMITTED 3
#define D_ABORTED   4

byte dstate[R]; byte dfpos[R]; byte dsize[R]; byte dtid[R];
byte ddpos[R];  bool dgcom[R];
byte covered[R];
bool boundary[MAXPOS];
byte g_frontier;
byte whint;
byte rdpos;
bool alive[NW];
byte nterm;
byte ncommitted;
byte ndelivered;

#define S(x)  dstate[(x)%R]
#define FP(x) dfpos[(x)%R]
#define SZ(x) dsize[(x)%R]
#define TD(x) dtid[(x)%R]
#define DP(x) ddpos[(x)%R]
#define GC(x) dgcom[(x)%R]
#define CV(x) covered[(x)%R]

/* claim predicate: r3 compares the full FREE(p) word; r1 (BUG1) only the state */
#ifdef BUG1
#define CLAIMABLE(x) (S(x) == D_FREE)
#else
#define CLAIMABLE(x) (S(x) == D_FREE && FP(x) == (x))
#endif

proctype Writer(byte me) {
  byte op = 0; byte need; byte p; byte hops; byte rp; byte q; byte i;
  byte gs; byte gf; byte gz; byte gt;
#ifdef BUG3
  byte hs; byte hz; byte ht; byte hq;
#endif

nextop:
  if
  :: atomic { alive[me] -> if :: need = 1 :: need = 2 fi }
  :: atomic { !alive[me] -> goto wdead }
  fi;

restart:
  if
  :: atomic { alive[me] -> p = whint; hops = 0 }
  :: atomic { !alive[me] -> goto wdead }
  fi;

walk:
  if
  :: atomic { alive[me] ->
       if
       :: S(p) == D_FREE ->
#ifdef BUG1
          goto cap                       /* r1: any FREE-state word is claimable */
#else
          if
          :: FP(p) == p -> goto cap      /* frontier */
          :: else -> goto restart        /* recycled slot: stale walk */
          fi
#endif
       :: S(p) == D_CLAIMED ->
#ifdef BUG3
          /* r1 helping: check owner liveness; if dead, recover it ourselves */
          goto help
#else
          goto restart                   /* back off; recovery is reader-only */
#endif
       :: else ->                        /* CLEARED/COMMITTED/ABORTED: vouched */
          p = p + SZ(p); hops++;
          if :: hops > R -> goto restart :: else -> skip fi
       fi }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  goto walk;

#ifdef BUG3
help:
  /* snapshot the dead claim (peek-time d, as r1 helpers did) */
  if
  :: atomic { alive[me] ->
       if
       :: S(p) == D_CLAIMED -> hs = S(p); hz = SZ(p); ht = TD(p)
       :: else -> goto restart           /* someone recovered it already */
       fi }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  if
  :: atomic { alive[me] && alive[ht] -> goto restart }   /* owner alive: back off */
  :: atomic { alive[me] && !alive[ht] -> skip }          /* license: owner dead */
  :: atomic { !alive[me] -> goto wdead }
  fi;
  hq = p + hz;
  /* delayed stamper: load g, then CAS -- possibly in a much later era */
  if
  :: atomic { alive[me] -> gs = S(hq); gf = FP(hq); gz = SZ(hq); gt = TD(hq) }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  if
  :: atomic { alive[me] ->
       if
       :: S(hq)==gs && FP(hq)==gf && SZ(hq)==gz && TD(hq)==gt ->
          assert(CV(hq) == 0);           /* S2: never stamp over a live claim */
          assert(boundary[hq]);          /* S4 */
          S(hq) = D_FREE; FP(hq) = hq; SZ(hq) = 0; TD(hq) = 0
       :: else -> skip                   /* CAS failed, fine */
       fi }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  if
  :: atomic { alive[me] ->
       if
       :: S(p)==hs && SZ(p)==hz && TD(p)==ht ->   /* CAS claim -> ABORTED */
          S(p) = D_ABORTED; FP(p) = 0; SZ(p) = hz; TD(p) = ht
       :: else -> skip
       fi }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  goto restart;
#endif

cap:
  if
  :: atomic { alive[me] -> rp = rdpos }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  if
  :: atomic { alive[me] ->
       if
       :: p < rp -> goto restart                       /* stale walk, NOT full */
       :: p >= rp && p + need + 1 - rp > R -> goto restart   /* full: retry */
       :: else -> skip
       fi }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  /* ---- claim CAS: expected FREE(p), i.e. word {FREE, p, 0, 0} ---- */
  if
  :: atomic { alive[me] ->
       if
       :: CLAIMABLE(p) ->
          assert(p == g_frontier);       /* Lemma 1 / S1: claims only at frontier */
          i = 0;
          do
          :: i < need -> assert(CV(p+i) == 0); CV(p+i) = 1; i++   /* S1 overlap */
          :: else -> break
          od;
          S(p) = D_CLAIMED; FP(p) = 0; SZ(p) = need; TD(p) = me;
          DP(p) = p; GC(p) = 0;
          g_frontier = p + need;
          boundary[p + need] = 1;
          q = p + need
       :: else -> goto restart           /* lost the CAS */
       fi }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  /* ---- successor stamp: load g, then CAS g -> FREE(q) ---- */
  if
  :: atomic { alive[me] -> gs = S(q); gf = FP(q); gz = SZ(q); gt = TD(q) }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  if
  :: atomic { alive[me] ->
       if
       :: S(q)==gs && FP(q)==gf && SZ(q)==gz && TD(q)==gt ->
          assert(CV(q) == 0);            /* S2 */
          assert(boundary[q]);           /* S4 */
          S(q) = D_FREE; FP(q) = q; SZ(q) = 0; TD(q) = 0
       :: else -> assert(false)          /* Lemma 2: no concurrent mutator */
       fi }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  /* ---- vouch CLEARED (full word store) ---- */
  if
  :: atomic { alive[me] -> S(p) = D_CLEARED; FP(p) = 0; SZ(p) = need; TD(p) = me }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  /* ---- publish hint ---- */
  if
  :: atomic { alive[me] -> assert(q <= g_frontier); whint = q }   /* S7 */
  :: atomic { !alive[me] -> goto wdead }
  fi;
  /* ---- fill payload ---- */
  if
  :: atomic { alive[me] -> skip }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  /* ---- outcome: commit full / abort / short commit ---- */
  if
  :: atomic { alive[me] ->
       if
       :: GC(p) = 1; S(p) = D_COMMITTED; FP(p) = 0; SZ(p) = need; TD(p) = me;
          ncommitted++; op++; goto opdone
       :: S(p) = D_ABORTED; FP(p) = 0; SZ(p) = need; TD(p) = me;
          op++; goto opdone
       :: need == 2 -> skip              /* short commit: used = 1 */
       fi }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  /* short commit: trailer FIRST (I1), then shrink size on the commit store */
  if
  :: atomic { alive[me] ->
       S(p+1) = D_ABORTED; FP(p+1) = 0; SZ(p+1) = need - 1; TD(p+1) = me;
       DP(p+1) = p + 1; GC(p+1) = 0;
       boundary[p+1] = 1 }
  :: atomic { !alive[me] -> goto wdead }
  fi;
  if
  :: atomic { alive[me] ->
       GC(p) = 1; S(p) = D_COMMITTED; FP(p) = 0; SZ(p) = 1; TD(p) = me;
       ncommitted++; op++ }
  :: atomic { !alive[me] -> goto wdead }
  fi;

opdone:
  if
  :: op < NOPS -> goto nextop
  :: else -> skip
  fi;
wdead:
  atomic { nterm++ }
}

active proctype Reader() {
  byte rd = 0; byte ds; byte dz; byte dt; byte q; byte i;
  byte gs; byte gf; byte gz; byte gt;

rloop:
  if
  :: atomic { S(rd) == D_FREE ->
       assert(FP(rd) == rd);             /* frontier word must be FREE(rd) */
       if
       :: nterm == NW ->
#ifndef NOA7
          assert(ndelivered == ncommitted);   /* every committed record delivered */
          assert(rd == g_frontier);           /* fully drained */
#endif
          goto rdone
       :: else -> skip                   /* empty; poll again */
       fi }
  :: atomic { S(rd) == D_COMMITTED ->
       assert(GC(rd));                   /* S3: fully committed */
       assert(DP(rd) == rd);             /* S8: this lap's record, exactly once */
       dz = SZ(rd); assert(dz > 0);
       assert(boundary[rd + dz]);        /* S4 */
       i = 0;
       do :: i < dz -> CV(rd+i) = 0; i++ :: else -> break od;
       ndelivered++;
       rd = rd + dz; rdpos = rd }
  :: atomic { S(rd) == D_ABORTED ->
       assert(DP(rd) == rd);             /* skipping this lap's record */
       dz = SZ(rd); assert(dz > 0);
       assert(boundary[rd + dz]);        /* S4 */
       i = 0;
       do :: i < dz -> CV(rd+i) = 0; i++ :: else -> break od;
       rd = rd + dz; rdpos = rd }
  :: atomic { (S(rd) == D_CLAIMED || S(rd) == D_CLEARED) ->
       ds = S(rd); dz = SZ(rd); dt = TD(rd) };   /* peek-time snapshot d */
     if
     :: atomic { alive[dt] -> goto rloop }       /* owner alive: busy, back off */
     :: atomic { !alive[dt] -> skip }            /* owner proven dead: recover */
     fi;
     assert(!alive[dt]);                 /* S5: recovery only for dead owners */
#ifdef FIXSTALE
     /* fix: owner is dead hence frozen; act on a FRESH read, not peek-time d */
     atomic { ds = S(rd); dz = SZ(rd); dt = TD(rd) };
     if
     :: ds == D_COMMITTED || ds == D_ABORTED -> goto rloop   /* owner finished */
     :: else -> skip
     fi;
#endif
     if
     :: ds == D_CLAIMED ->               /* owner never stamped its successor */
#ifndef BUG2
        q = rd + dz;
        atomic { gs = S(q); gf = FP(q); gz = SZ(q); gt = TD(q) };
        atomic {
          if
          :: S(q)==gs && FP(q)==gf && SZ(q)==gz && TD(q)==gt ->
             assert(CV(q) == 0);         /* S2 */
             assert(boundary[q]);        /* S4 */
             S(q) = D_FREE; FP(q) = q; SZ(q) = 0; TD(q) = 0
          :: else -> assert(false)       /* spec's assert(ok), Lemma 2 */
          fi };
#endif
        skip
     :: else -> skip                     /* CLEARED: successor already stamped */
     fi;
     /* ABORTED store = with_state(d, ABORTED): full word from snapshot d */
     atomic { S(rd) = D_ABORTED; FP(rd) = 0; SZ(rd) = dz; TD(rd) = dt };
     goto rloop
  fi;
  goto rloop;
rdone:
  skip
}

active proctype Reaper() {
end:
  do
  :: atomic { alive[0] -> alive[0] = 0 }
  :: atomic { alive[1] -> alive[1] = 0 }
#if NW > 2
  :: atomic { alive[2] -> alive[2] = 0 }
#endif
  od
}

init {
  atomic {
    boundary[0] = 1;
    alive[0] = 1; alive[1] = 1;
#if NW > 2
    alive[2] = 1;
#endif
    run Writer(0); run Writer(1);
#if NW > 2
    run Writer(2);
#endif
  }
}
