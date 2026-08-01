#include "pgt/mpsc/spsc_ring.hh"

static_assert(pgt::mpsc::QueueLike<pgt::mpsc::SpscRing<>>);
