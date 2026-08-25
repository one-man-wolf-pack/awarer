# Changelog

## [0.3.1](https://github.com/one-man-wolf-pack/awarer/compare/v0.3.0...v0.3.1) (2026-08-25)


### Bug Fixes

* **build:** let golangci-lint own staticcheck outright ([9b452f8](https://github.com/one-man-wolf-pack/awarer/commit/9b452f84440514fceb9f23d16caefe99524bc444))
* **build:** prefer the Go 1.27.0 toolchain ([789c6b9](https://github.com/one-man-wolf-pack/awarer/commit/789c6b94430dced952ad1c192d2505f180c6b20f))
* **ci:** state what the FreeBSD lane's local toolchain must satisfy ([87acca1](https://github.com/one-man-wolf-pack/awarer/commit/87acca19bc8000673e94365978230c951805debe))


### Dependencies

* update modernc.org/sqlite to v1.57.0 ([a89606b](https://github.com/one-man-wolf-pack/awarer/commit/a89606b1a235ad94df8e116972dc3f259c5ede5b))
* update the cpuid and modernc indirect requirements ([a5b09b3](https://github.com/one-man-wolf-pack/awarer/commit/a5b09b315109bc5f2a8970058034abf2c8f1965b))


### Documentation

* state the macOS support boundary each install path actually carries ([afd9901](https://github.com/one-man-wolf-pack/awarer/commit/afd9901408e67df9aa0d62e543b5654da31931d0))

## [0.3.0](https://github.com/one-man-wolf-pack/awarer/compare/v0.2.0...v0.3.0) (2026-08-16)


### Features

* **release:** build the Homebrew package from released source ([ff0d619](https://github.com/one-man-wolf-pack/awarer/commit/ff0d61921e8b0dd29ee5805ca009de1ea9b82a08))


### Bug Fixes

* **build:** require Go 1.26.6 ([be31bf6](https://github.com/one-man-wolf-pack/awarer/commit/be31bf65e22ca1abc5e32e8ae9df2b55e1fb3308))
* **ci:** separate Go floor from patched toolchain ([c1ebcec](https://github.com/one-man-wolf-pack/awarer/commit/c1ebcec3a4b9369f08579623b93c9a79b807eb40))

## [0.2.0](https://github.com/one-man-wolf-pack/awarer/compare/v0.1.3...v0.2.0) (2026-08-12)


### Features

* **run:** select effect roots by exact project-relative path ([9b3ea67](https://github.com/one-man-wolf-pack/awarer/commit/9b3ea67864644ae9ab8cf7602ff5024266a9fe78))

## [0.1.3](https://github.com/one-man-wolf-pack/awarer/compare/v0.1.2...v0.1.3) (2026-08-11)


### Bug Fixes

* **windows:** canonicalize followed paths consistently ([5cc2ef0](https://github.com/one-man-wolf-pack/awarer/commit/5cc2ef01799f611fb8bda9dc4370a45105df2abf))
* **worktreefs:** keep every followed hop accounted while canonicalizing ([ec250fa](https://github.com/one-man-wolf-pack/awarer/commit/ec250faa09126ab770257b8cea4c445dba58d23c))


### Performance Improvements

* **blake3hash:** reuse cleared copy scratch when hashing a reader ([cc78da1](https://github.com/one-man-wolf-pack/awarer/commit/cc78da1634451f98510665d6b50a68a188dc65e3))
* **blobstore:** verify an existing blob through the shared hashing scratch ([1387be3](https://github.com/one-man-wolf-pack/awarer/commit/1387be304c79e989106a86933bf8c5aa2edc639f))
* **checkpoint:** bound habitual checkpoint history windows ([85b57e5](https://github.com/one-man-wolf-pack/awarer/commit/85b57e58a34e245ee8f7b72c45d25f2a6ae6c958))
* **checkpoint:** retain content sources only where provenance cannot be rebuilt ([0737e6e](https://github.com/one-man-wolf-pack/awarer/commit/0737e6eab4fc56ca3ef8626a9f698504790464d1))
* **doctor:** bound the nested-marker scan's listings and frontier ([d95447b](https://github.com/one-man-wolf-pack/awarer/commit/d95447b26565150d8f7fca15ef7598f545a82aff))
* **doctor:** verify each unique checkpoint blob once per invocation ([107aabc](https://github.com/one-man-wolf-pack/awarer/commit/107aabc0c6c307b2acb5e8d67b2762819351f274))
* **worktreefs:** reuse effective ignore matchers across directories ([1620d1f](https://github.com/one-man-wolf-pack/awarer/commit/1620d1fcd4e38625f9a7b469dc2c3701bbb82af1))


### Documentation

* trim the followed-path fixture comments to their mandatory rationale ([846ae03](https://github.com/one-man-wolf-pack/awarer/commit/846ae03ac21c49ca0687ea8b743d234f6e683c0a))


### Code Refactoring

* **checkpoint:** give checkpoint history order one domain owner ([c3ed419](https://github.com/one-man-wolf-pack/awarer/commit/c3ed4191cdbadaf262fa64296b403eec4919d7db))
* **fsx:** give bounded directory streaming a descriptor-taking owner ([21115dd](https://github.com/one-man-wolf-pack/awarer/commit/21115ddf2a827d971423a27e5cf221376cb9b4f4))

## [0.1.2](https://github.com/one-man-wolf-pack/awarer/compare/v0.1.1...v0.1.2) (2026-08-08)


### Documentation

* **install:** explain macOS quarantine handling ([0d39cab](https://github.com/one-man-wolf-pack/awarer/commit/0d39cab834c1311cc1f3f349b2c3b9fb5e03e222))

## [0.1.1](https://github.com/one-man-wolf-pack/awarer/compare/v0.1.0...v0.1.1) (2026-08-08)


### Bug Fixes

* **stateprovider:** preserve the skipped count of a state comparison ([35dad98](https://github.com/one-man-wolf-pack/awarer/commit/35dad98e0cad8c8ddd6b4f99bddbfe8d549509cf))
* **stateprovider:** propagate marked internal faults instead of io-error ([1a76867](https://github.com/one-man-wolf-pack/awarer/commit/1a768678e033fe32da0d040c50280d76c87dfbc3))


### Documentation

* **install:** document the Homebrew cask and drop the ownership story ([094f84d](https://github.com/one-man-wolf-pack/awarer/commit/094f84dcbdbce4f988ee141f133d56aaee7c6ba5))
* **install:** stop denying that another distribution channel exists ([842fa8b](https://github.com/one-man-wolf-pack/awarer/commit/842fa8bc3e00aafdd6df91e6c726ec4411cbf614))
* remove the public maintainer runbooks and pin ledgers ([8f1d47b](https://github.com/one-man-wolf-pack/awarer/commit/8f1d47b917a9e0186d09fe1f230e09a317505b79))


### Tests

* **manifestsort:** remove the retained-heap ratio oracle ([21480b1](https://github.com/one-man-wolf-pack/awarer/commit/21480b1367082bb3af297358dde27634f136d6eb))
* **runevidence:** remove the heap sampler boundedness oracle ([23aac15](https://github.com/one-man-wolf-pack/awarer/commit/23aac15e930b2902aff5e1e515ad853f0b584a53))
* **stateprovider:** replace heap boundedness oracles with deterministic evidence ([4f2240e](https://github.com/one-man-wolf-pack/awarer/commit/4f2240e5822c16fa713254427c6258101aaf50fa))
* **fixtures:** make same-size rewrites visible to the scan ([59a5cca](https://github.com/one-man-wolf-pack/awarer/commit/59a5cca65b3b80d7c8e86ecf3686576d0b6e349b))
* **stress:** race gc against readers over a churned history ([07dbbf0](https://github.com/one-man-wolf-pack/awarer/commit/07dbbf052a1e53ca7a4d8371b8f8f20687ab3be1))


### Build System

* **deps:** Bump vmactions/freebsd-vm in the actions group ([ebf851e](https://github.com/one-man-wolf-pack/awarer/commit/ebf851ee5e6d6d41b5c12049043bb5461a628c58))
* **deps:** update the go dependency group ([eec0b7e](https://github.com/one-man-wolf-pack/awarer/commit/eec0b7ebddadcac3c43e744c7da93115e8d9b58f))
* **release:** publish a Homebrew cask from the release job ([db88186](https://github.com/one-man-wolf-pack/awarer/commit/db88186858e1994b27061944a747811e9f50dd36))
* **release:** strip the symbol table and DWARF from release binaries ([622182f](https://github.com/one-man-wolf-pack/awarer/commit/622182fb0401918afe71e64dcd78cc1e79ffaf36))

## 0.1.0 (2026-08-07)


### Features

* publish awa source baseline ([7947a5a](https://github.com/one-man-wolf-pack/awarer/commit/7947a5a7b050643671774a4ad66d85141098f70f))
