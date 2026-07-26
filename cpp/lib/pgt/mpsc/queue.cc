#include "pgt/mpsc/queue.hh"

// The variants are templates over their policy, so the definitions live in the
// header. This TU pins the default instantiations and checks the contract, so
// a protocol change that breaks the API surfaces here rather than at first use.

namespace pgt::mpsc {

// SECTION: Ring (owner: impl-queue)
template class Ring<DefaultPolicy>;
template class Ring<BackoffPolicy>;

static_assert(QueueLike<Ring<DefaultPolicy>>);
static_assert(QueueLike<Ring<BackoffPolicy>>);
static_assert(QueueLike<Mpsc>);

// SECTION: SpscRing / Sharded instantiations (owner: impl-spsc)
template class SpscRing<DefaultPolicy>;
template class SpscRing<BackoffPolicy>;
template class Sharded<Ring<DefaultPolicy>>;
template class Sharded<SpscRing<DefaultPolicy>>;

static_assert(QueueLike<SpscRing<DefaultPolicy>>);
static_assert(QueueLike<SpscRing<BackoffPolicy>>);
static_assert(QueueLike<ShardedMpsc>);
static_assert(QueueLike<MultiSpsc>);

}  // namespace pgt::mpsc
