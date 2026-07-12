# Nested-Array Serialization Spike

Status: completed research spike; no production format selected.

## Question

Can an existing IDL provide all of the following without exposing the FlatBuffers builder API or Protobuf's owning object tree?

- generated C++, Go, Python, and TypeScript/Deno consumers;
- nested records and arrays;
- allocation-free borrowed reads in C++;
- contiguous fixed-record arrays;
- caller-owned encoding buffers;
- stable field IDs and governed schema evolution;
- a conventional, readable application API.

## Workload

The representative message is an order book containing scalar identity fields, nested metadata, and two arrays of 1,000 fixed-size levels. The proposed minimal schema is [book.bview](book.bview).

## Findings

| Candidate | Language matrix | Borrowed nested reads | API | Result |
| --- | --- | --- | --- | --- |
| Apache Fory IDL | C++, Go, Python, JS/TS | No; normal IDL decode creates owning native objects | Conventional | Reject |
| Apache Fory Row | C++, Python, Rust, Java documented | Yes | View-oriented | Reject: no documented Go or JS/TS row support |
| Bebop | C++, Python, TS and others | No; arrays and records decode into owning collections | Conventional | Reject |
| FlatBuffers | Complete | Yes | Builder/accessor API rejected by design preference | Reject |
| Minimal borrowed view | Must be implemented | Yes | Can be made conventional | Keep only as a falsifier/prototype |

### Fory

Fory's IDL is pleasant and supports dense numeric `array<T>` separately from general `list<T>`, but its generated C++ maps strings and collections to owning standard-library types. Deserialization therefore materializes an object tree. Fory's separate Row format advertises random access to fields, nested values, and arrays without whole-object deserialization, but the documented multi-language Row set does not cover Go and JavaScript/TypeScript. The normal IDL path and Row path are not one portable borrowed-view API across the required matrix.

Sources: [Fory compiler](https://fory.apache.org/docs/compiler/), [generated code](https://fory.apache.org/docs/compiler/generated_code/), [overview](https://fory.apache.org/docs/introduction/overview/), [C++ row format](https://fory.apache.org/docs/1.0.0/guide/cpp/row_format/).

### Bebop

Bebop has the cleanest candidate schema syntax and conventional generated encode/decode APIs. Its wire arrays are a count followed by element encodings, but generated decoding still materializes language records and collections. Current documentation is also internally inconsistent for Go: the landing material lists Go while the compiler configuration reference does not list a built-in Go generator alias. Deno is not an explicitly documented runtime target.

Sources: [Bebop documentation](https://docs.bebop.sh/), [compiler configuration](https://docs.bebop.sh/reference/bebop-json/), [wire format](https://docs.bebop.sh/reference/wire-format/).

## Runnable Lower Bound

[borrowed_view.cpp](borrowed_view.cpp) is not a proposed production format. It is a compact test of the required C++ API and allocation floor:

```cpp
book_view book{encoded};

for (std::size_t i = 0; i < book.bids().size(); ++i) {
  consume(book.bids()[i].price(), book.bids()[i].quantity());
}
```

It uses one relocatable byte buffer, offset/count descriptors, a nested metadata view containing a borrowed string, and contiguous 16-byte level wire records. These are not exposed as a potentially unaligned `std::span<Level>`: each scalar is read through `memcpy`-based little-endian loads rather than pointer-punning wire structs.

Local validation on the available Debian GCC 14.2 host:

```text
g++ -std=gnu++23 -O3 -Wall -Wextra -Werror borrowed_view.cpp

wire_bytes=32084
decode_allocations=0
iterations=100000
levels_per_side=1000
ns_per_iteration=695
```

The timing is only a smoke measurement, not a publishable benchmark: it is a single run on an uncontrolled workstation. The allocation result is the useful evidence. Constructing and repeatedly traversing borrowed views performed zero calls to the prototype's overridden ordinary `operator new` after creation of the encoded buffer; the counter is not a general-purpose allocator tracer and does not intercept every aligned or nothrow allocation form.

## Minimal Custom Format Boundary

A custom format is justifiable only if it stays narrow:

- little-endian immutable buffers;
- fixed records, variable records, strings, bytes, and one-dimensional arrays;
- no maps, object references, cycles, services, in-place mutation, or runtime reflection initially;
- one validation pass followed by cheap borrowed accessors;
- generated owning values are optional adapters, never the wire API;
- fixed records in arrays remain contiguous;
- variable records use offset/length descriptors;
- deleted field IDs remain reserved;
- only trailing optional-field additions are initially compatible.

Expected generated surfaces:

```text
C++         VerifiedBuffer, BookView, span-like LevelArrayView, encode_into
Go          Buffer, BookView, borrowed []byte accessors, EncodeInto
Python      memoryview-backed BookView and sequence views
TypeScript  Uint8Array/DataView-backed BookView and EncodeInto
```

Text decoding necessarily allocates in some languages. Generated APIs should expose `venue_bytes`/`labelBytes` alongside decoded string conveniences rather than claiming that strings are universally zero-copy.

## Decision

Do not adopt Fory or Bebop for this requirement. Neither normal generated model provides borrowed nested reads across the full language matrix. Do not adopt a custom wire format yet either.

The next implementation gate is a real vertical slice of the minimal borrowed-view design:

1. parser, validator, and stable IR for [book.bview](book.bview);
2. C++, Go, Python, and TypeScript emitters;
3. one canonical byte fixture consumed by every runtime;
4. malformed/truncated-buffer tests;
5. old-reader/new-writer compatibility fixtures;
6. allocation and traversal benchmarks against Protobuf arenas, Fory, Bebop, and FlatBuffers;
7. an ownership cap of roughly 2,000–3,000 non-generated shared lines.

Adopt the custom format only if it materially wins the actual borrowed-access workload and remains small enough to audit completely. Otherwise accept an owning IDL runtime and optimize only the measured hot boundary.

## Execution Gaps

Candidate code generation did not run in this environment. Go and Deno are absent, Fory's compiler was not installed, and candidate downloads required network installation that was not completed. Consequently, language support and generated-model conclusions above are grounded in current official documentation; only the minimal C++ borrowed-view prototype was compiled and executed locally.
