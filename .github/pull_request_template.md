## What this changes

<!-- One or two sentences. What is different afterwards? -->

## Why

<!-- What was wrong, or what could not be done before. -->

## How it was verified

<!--
Not "tests pass" — what did you actually check?

If it touches the engine, a test that fails without the change is the most
convincing thing you can offer. If it touches reference data, a citation is.
-->

---

- [ ] `make check` passes (`gofmt`, `go vet`, `go test`)
- [ ] New behaviour has a test that would fail without it
- [ ] Reference-data changes cite a source
- [ ] Anything that changes what a number *means* is reflected in the docs
