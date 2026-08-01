#include "pgt/mpsc/mpsc_ring.hh"

static_assert(pgt::mpsc::QueueLike<pgt::mpsc::MpscRing<>>);
