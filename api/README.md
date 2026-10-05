# gentian-os/api

What another program needs in order to talk to a Gentian cluster or read what
one produced, as a Go module of its own:

| Package | What it is |
|---|---|
| `v1alpha1` | the resource types of `gentianos.io/v1alpha1`, from which the CRDs are generated |
| `bundle` | the index of a tenant bundle: the manifest and the unencrypted header |
| `statement` | the payload of an entitlement statement, as an App Store signs it |

```sh
go get github.com/gentian-org/gentian-os/api
```

It imports nothing from the rest of the repository, and the rest of the
repository builds against it through a `replace` in the root `go.mod`. From a
checkout, `go.work` joins the two, so `go test ./api/...` works from the root.

## License

Apache-2.0 — see [LICENSE](LICENSE). This directory is licensed separately
from the repository around it, so that anything may be written against these
types and formats.
