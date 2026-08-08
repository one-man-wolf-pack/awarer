# Changelog

## [0.2.0](https://github.com/one-man-wolf-pack/awarer/compare/v0.1.1...v0.2.0) (2026-08-08)


### Features

* publish awa source baseline ([7947a5a](https://github.com/one-man-wolf-pack/awarer/commit/7947a5a7b050643671774a4ad66d85141098f70f))


### Bug Fixes

* **release:** remove duplicate changelog heading ([37182b5](https://github.com/one-man-wolf-pack/awarer/commit/37182b5bd605c9d9213003518e1206aef2be9c14))
* **stateprovider:** preserve the skipped count of a state comparison ([35dad98](https://github.com/one-man-wolf-pack/awarer/commit/35dad98e0cad8c8ddd6b4f99bddbfe8d549509cf))
* **stateprovider:** propagate marked internal faults instead of io-error ([1a76867](https://github.com/one-man-wolf-pack/awarer/commit/1a768678e033fe32da0d040c50280d76c87dfbc3))


### Documentation

* **install:** document the Homebrew cask and drop the ownership story ([094f84d](https://github.com/one-man-wolf-pack/awarer/commit/094f84dcbdbce4f988ee141f133d56aaee7c6ba5))
* **install:** stop denying that another distribution channel exists ([842fa8b](https://github.com/one-man-wolf-pack/awarer/commit/842fa8bc3e00aafdd6df91e6c726ec4411cbf614))
* remove the public maintainer runbooks and pin ledgers ([8f1d47b](https://github.com/one-man-wolf-pack/awarer/commit/8f1d47b917a9e0186d09fe1f230e09a317505b79))


### Tests

* **fixtures:** make same-size rewrites visible to the scan ([59a5cca](https://github.com/one-man-wolf-pack/awarer/commit/59a5cca65b3b80d7c8e86ecf3686576d0b6e349b))
* **manifestsort:** remove the retained-heap ratio oracle ([21480b1](https://github.com/one-man-wolf-pack/awarer/commit/21480b1367082bb3af297358dde27634f136d6eb))
* **runevidence:** remove the heap sampler boundedness oracle ([23aac15](https://github.com/one-man-wolf-pack/awarer/commit/23aac15e930b2902aff5e1e515ad853f0b584a53))
* **stateprovider:** replace heap boundedness oracles with deterministic evidence ([4f2240e](https://github.com/one-man-wolf-pack/awarer/commit/4f2240e5822c16fa713254427c6258101aaf50fa))
* **stress:** race gc against readers over a churned history ([07dbbf0](https://github.com/one-man-wolf-pack/awarer/commit/07dbbf052a1e53ca7a4d8371b8f8f20687ab3be1))


### Build System

* **deps:** Bump vmactions/freebsd-vm in the actions group ([ebf851e](https://github.com/one-man-wolf-pack/awarer/commit/ebf851ee5e6d6d41b5c12049043bb5461a628c58))
* **deps:** update the go dependency group ([eec0b7e](https://github.com/one-man-wolf-pack/awarer/commit/eec0b7ebddadcac3c43e744c7da93115e8d9b58f))
* **release:** publish a Homebrew cask from the release job ([db88186](https://github.com/one-man-wolf-pack/awarer/commit/db88186858e1994b27061944a747811e9f50dd36))
* **release:** strip the symbol table and DWARF from release binaries ([622182f](https://github.com/one-man-wolf-pack/awarer/commit/622182fb0401918afe71e64dcd78cc1e79ffaf36))


### Miscellaneous Chores

* **main:** release 0.1.0 ([5eb9fed](https://github.com/one-man-wolf-pack/awarer/commit/5eb9fed53b51fd1c19a257b0e2978eb34dc8678f))
* **main:** release 0.1.1 ([0d802c5](https://github.com/one-man-wolf-pack/awarer/commit/0d802c5b5c0a98b062f59b1371aaff089c3e6d7e))

## [0.1.1](https://github.com/one-man-wolf-pack/awarer/compare/v0.1.0...v0.1.1) (2026-08-08)


### Bug Fixes

* **stateprovider:** preserve the skipped count of a state comparison ([35dad98](https://github.com/one-man-wolf-pack/awarer/commit/35dad98e0cad8c8ddd6b4f99bddbfe8d549509cf))
* **stateprovider:** propagate marked internal faults instead of io-error ([1a76867](https://github.com/one-man-wolf-pack/awarer/commit/1a768678e033fe32da0d040c50280d76c87dfbc3))


### Documentation

* **install:** document the Homebrew cask and drop the ownership story ([13ac4ec](https://github.com/one-man-wolf-pack/awarer/commit/13ac4ec19ef7bdeb4d4c1c580c40830860e5378b))
* **install:** stop denying that another distribution channel exists ([8b414c8](https://github.com/one-man-wolf-pack/awarer/commit/8b414c89df1c27d4ccb776659dca4353a1902607))
* remove the public maintainer runbooks and pin ledgers ([21a9fc2](https://github.com/one-man-wolf-pack/awarer/commit/21a9fc2e9b29cc45dae638c9149dc66dbb476569))


### Tests

* **manifestsort:** remove the retained-heap ratio oracle ([21480b1](https://github.com/one-man-wolf-pack/awarer/commit/21480b1367082bb3af297358dde27634f136d6eb))
* **runevidence:** remove the heap sampler boundedness oracle ([23aac15](https://github.com/one-man-wolf-pack/awarer/commit/23aac15e930b2902aff5e1e515ad853f0b584a53))
* **stateprovider:** replace heap boundedness oracles with deterministic evidence ([4f2240e](https://github.com/one-man-wolf-pack/awarer/commit/4f2240e5822c16fa713254427c6258101aaf50fa))


### Build System

* **deps:** Bump vmactions/freebsd-vm in the actions group ([ebf851e](https://github.com/one-man-wolf-pack/awarer/commit/ebf851ee5e6d6d41b5c12049043bb5461a628c58))
* **deps:** update the go dependency group ([d047839](https://github.com/one-man-wolf-pack/awarer/commit/d047839eda196176de6e9fa1e9a87b6cd420dd1e))
* **release:** publish a Homebrew cask from the release job ([eef0d5e](https://github.com/one-man-wolf-pack/awarer/commit/eef0d5e4121a6b60a4022b9cb27ad46e63506e08))
* **release:** strip the symbol table and DWARF from release binaries ([523d153](https://github.com/one-man-wolf-pack/awarer/commit/523d1539183d21ecda09090cd3a392b5e306c11d))

## 0.1.0 (2026-08-07)


### Features

* publish awa source baseline ([7947a5a](https://github.com/one-man-wolf-pack/awarer/commit/7947a5a7b050643671774a4ad66d85141098f70f))
