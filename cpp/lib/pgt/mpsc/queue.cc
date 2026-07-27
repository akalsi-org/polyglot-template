#include "../mpsc/queue.hh"

// The variants are templates over their policy, so the definitions live in the
// header. This TU pins the default instantiations and checks the contract, so
// a protocol change that breaks the API surfaces here rather than at first use.

namespace pgt::mpsc {

// SECTION: SpscRing instantiations (owner: impl-spsc)
template class SpscRing<DefaultPolicy>;
template class SpscRing<BackoffPolicy>;

static_assert(QueueLike<SpscRing<DefaultPolicy>>);
static_assert(QueueLike<SpscRing<BackoffPolicy>>);
static_assert(QueueLike<Spsc>);

}  // namespace pgt::mpsc
