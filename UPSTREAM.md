# Upstream provenance

- Project: [schollz/croc](https://github.com/schollz/croc)
- Base: [`b6d9a35fe63e05c67ca2f4a3e6e2320c07f6edff`](https://github.com/schollz/croc/commit/b6d9a35fe63e05c67ca2f4a3e6e2320c07f6edff)
- License: MIT; see the preserved [LICENSE](LICENSE).
- [Original README at the fork point](https://github.com/schollz/croc/blob/b6d9a35fe63e05c67ca2f4a3e6e2320c07f6edff/README.md)

Flock's changes add the `cmd/flock` entry point, a JSON agent contract, a native
QUIC file-data adapter, and an explicit Flock capability/transport extension.
The upstream croc entry point, transport implementations, module path, and
license remain in the source tree. Inherited croc packaging and release workflows
are not Flock distribution instructions; Flock has no release process yet.

An `upstream` Git remote can be added with:

```sh
git remote add upstream https://github.com/schollz/croc.git
```
