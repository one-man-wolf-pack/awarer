# Changelog

## [0.4.1](https://github.com/one-man-wolf-pack/awarer/compare/v0.4.0...v0.4.1) (2026-10-11)


### Bug Fixes

* **run:** match --command against the full recorded command line ([4c3336c](https://github.com/one-man-wolf-pack/awarer/commit/4c3336c0b8080476cea5361ba8974d18fd73092a))


### Miscellaneous Chores

* share this checkout's awa policy and refresh the agent guidance ([17dcf9e](https://github.com/one-man-wolf-pack/awarer/commit/17dcf9ed8e18997bd0431b2c5246c778ffb60502))

## [0.4.0](https://github.com/one-man-wolf-pack/awarer/compare/v0.3.2...v0.4.0) (2026-10-10)


### ⚠ BREAKING CHANGES

* **patterns:** the same pattern text can select different paths than before. A `?` now excludes or keeps hash-only more than it did, and an inner-slash pattern such as `config/*.env` no longer applies below the project root, so nested files it used to cover are scanned and, for hash_only_patterns, store their bytes again.
* **restore:** restore deletes a path only when the source's recorded coverage proves it absent, and refuses a create whose destination exists but is hidden. Checkpoints, runs, and recovery observations from earlier releases stay readable: their positive entries, deltas, and owned bytes remain usable, but they cannot prove absence, so dependent deletions, `--all`, and undo from such a record can refuse with `absence-unproved`. `awa state compare` reports `incomparable` / `absence-unproved` for distinct earlier observations, including both sides of one run, even when their tree hashes match; the same record still compares equal. New records keep excluded path names (never their content), and older awa binaries are not guaranteed to read the new record formats.
* **run:** `audit` is now a reserved `awa run` subcommand. To run an executable named `audit` through awa, use `awa run -- audit`.

### Features

* **changes:** disclose the observation-delta basis and recorded scan configs ([c85da5f](https://github.com/one-man-wolf-pack/awarer/commit/c85da5fdad11cec2a406949515d88f02eb2af330))
* **checkpoint:** keep selected files hash-only, env files by default ([8fedd4a](https://github.com/one-man-wolf-pack/awarer/commit/8fedd4a905fb47054dbe96c9eaa4d7470c999cc9))
* **doctor:** advise when the .awa store reaches a configured size ([30d2be9](https://github.com/one-man-wolf-pack/awarer/commit/30d2be991c43e3ba40aa00429803fac638368b50))
* **doctor:** audit watched effect roots with --capacity ([fc09364](https://github.com/one-man-wolf-pack/awarer/commit/fc09364727a159ffb88bfaf33d5d502f7893b4cd))
* **doctor:** diagnose and repair headerless checkpoint publication remnants ([aeef9fc](https://github.com/one-man-wolf-pack/awarer/commit/aeef9fc67edcf1a538b27e5eb95f90cbced9cfe7))
* **fsx:** size an open directory under native names with skipped subtrees ([889f041](https://github.com/one-man-wolf-pack/awarer/commit/889f041c93ae1676b240461dd031881ad64de4c1))
* **run:** accept --stdin null on run and command-mode explain ([15a319a](https://github.com/one-man-wolf-pack/awarer/commit/15a319a202561da0d225beaeb19e0b42d92d8894))
* **run:** audit recorded executions by primary stored reason ([63f1cab](https://github.com/one-man-wolf-pack/awarer/commit/63f1cab1d3c6d49954baf10a6df33cf468ab10dd))


### Bug Fixes

* **changes:** label a recovery operand in comparison JSON ([0b272c4](https://github.com/one-man-wolf-pack/awarer/commit/0b272c488b90a2a74afd2230abfa6077e5aded2e))
* **changes:** label comparison ignore sources as current configuration ([1f18e56](https://github.com/one-man-wolf-pack/awarer/commit/1f18e569d7b8aad39c276c95b6396adb8f647e61))
* **doctor:** report Ctrl+C during the repair lock wait as an interruption ([4f215a0](https://github.com/one-man-wolf-pack/awarer/commit/4f215a08b9ef53b3d0013e9cc5fee2fd47a6545a))
* **gc:** account logical bytes of whole deletion boundaries ([cd9a2a9](https://github.com/one-man-wolf-pack/awarer/commit/cd9a2a99f3ff59ddfb1befd90b7c562c8b68c7e2))
* **lockfile:** treat a lock released during a scan as gone, not unknown ([4602ba5](https://github.com/one-man-wolf-pack/awarer/commit/4602ba5cf7a1caf45c6927f5503d992ee4697388))
* **manifest:** keep a read failure delivered with the final row ([bea0796](https://github.com/one-man-wolf-pack/awarer/commit/bea0796493c143506a2e329c5e0c4af73868ce9d))
* **manifestsort:** fail a spill merge on a run read failure ([f242579](https://github.com/one-man-wolf-pack/awarer/commit/f242579545701a605b643246201222f92955471d))
* **patterns:** match exclusions and retention with Git's pattern format ([9450f0d](https://github.com/one-man-wolf-pack/awarer/commit/9450f0dc4271495b8e6d41071ee4fb401a7a5eb6))
* **restore:** prove path absence from recorded coverage ([03e7883](https://github.com/one-man-wolf-pack/awarer/commit/03e7883702a301767e2ea2ff5284761f65e09cdd))
* **restore:** refuse directory removal over known surviving children before mutation ([5a0f4ec](https://github.com/one-man-wolf-pack/awarer/commit/5a0f4ec7e0323dfe4bc247e1824ff95bafeff09e))


### Dependencies

* move the licence manifest to modernc.org/sqlite v1.60.1 ([d0452d1](https://github.com/one-man-wolf-pack/awarer/commit/d0452d13a67dfbc2cd3f893b55c60659226d37a6))


### Documentation

* **help:** add a maintenance topic for reviewing an existing setup ([baeffa7](https://github.com/one-man-wolf-pack/awarer/commit/baeffa7ac4cbeedbd73eefa579eeeab912b37fff))


### Tests

* **acceptance:** stop leaking build dirs and key cached results to the binary's sources ([8d06773](https://github.com/one-man-wolf-pack/awarer/commit/8d067732b5d4a0b252a65701a71b757fd4c699ff))


### Build System

* **deps:** Bump modernc.org/sqlite ([ec3a2ac](https://github.com/one-man-wolf-pack/awarer/commit/ec3a2aca7093274ebab11eddecc4f9d6044b1f92))
* **deps:** Bump vmactions/freebsd-vm ([8232031](https://github.com/one-man-wolf-pack/awarer/commit/8232031856009a810f954da4cdf116c6f08304bf))
* **lint:** stop requiring package comments ([352ba3a](https://github.com/one-man-wolf-pack/awarer/commit/352ba3aaf379d818fdafe3a6f8f3e6449a7beda2))
* use the go1.27.2 toolchain ([043fbcb](https://github.com/one-man-wolf-pack/awarer/commit/043fbcbe11e1b5df2148ef59121c59fb750df18d))


### Continuous Integration

* run the portable stdin wiring test on Windows and FreeBSD ([553a1b2](https://github.com/one-man-wolf-pack/awarer/commit/553a1b241e5f4244f18164af6dfa05896511d61d))

## [0.3.2](https://github.com/one-man-wolf-pack/awarer/compare/v0.3.1...v0.3.2) (2026-09-28)


### Dependencies

* move the licence manifest to the go-dependencies group bump ([4d19e8d](https://github.com/one-man-wolf-pack/awarer/commit/4d19e8d40d698790b2fc381ae43adffec026a0ec))
* update modernc.org/sqlite to v1.60.0 and the Go toolchain to go1.27.1 ([cdec422](https://github.com/one-man-wolf-pack/awarer/commit/cdec42284b563903a9fc239c4665ceec7aab9afd))


### Build System

* **deps:** Bump the go-dependencies group with 3 updates ([89f9ecb](https://github.com/one-man-wolf-pack/awarer/commit/89f9ecb029e315e58e082e54d2ab7b1ce0509d2f))
* **deps:** Bump vmactions/freebsd-vm in the actions group ([744bdb8](https://github.com/one-man-wolf-pack/awarer/commit/744bdb879bb42a4d6461cfa3dfda8ba89412d69f))
* update the pinned tool and action versions ([0882e05](https://github.com/one-man-wolf-pack/awarer/commit/0882e0506197b7e701970182e27ae4ac6ff28d16))

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
