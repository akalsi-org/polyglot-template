import sys, gc
import vcall_demo as m

class Boxed:
  def __init__(self, v): self.v = v
  def __str__(self): return str(self.v)

items = [Boxed(i) for i in range(300)]

gc.collect()
before = sys.getallocatedblocks()
for _ in range(300):
  m.prefix_join(items, prefix="x=")
gc.collect()
after = sys.getallocatedblocks()
delta = after - before
print("allocated-block delta after 300x prefix_join(300 items):", delta)
# A leaking VSTEAL(joined)-without-release pattern leaks one str object
# per loop iteration but the last, i.e. ~299 objects per call * 300
# calls = ~89700 permanently-live blocks. A fixed VMOVE build should
# show a delta that does not scale with call count (bounded, near-zero
# after GC reclaims all the scratch intermediate strings).
