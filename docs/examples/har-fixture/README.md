# HAR fixture example

This command demonstrates how `hario` and `hartest` compose without adding
fixture concerns to the core recorder API. It is documentation code, not an
importable helper package.

Replace the URL in `main.go` with a request present in your capture, then run:

```sh
go run ./docs/examples/har-fixture ./capture.har
```

The `http.Client` cannot reach the network: a request that does not match an
unused fixture returns an error.
