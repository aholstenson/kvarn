# Changelog

## 0.1.0 (2026-05-22)


### Features

* Ability to control max steps via config ([715ec9d](https://github.com/aholstenson/kvarn/commit/715ec9d546b0fb5a7718e34901f3173c348ab318))
* Add authentication support ([36ba446](https://github.com/aholstenson/kvarn/commit/36ba44643128fbc770a31f9146c98b67790ba577))
* Add internal task planning tools for LLM agent ([29d8371](https://github.com/aholstenson/kvarn/commit/29d8371b5e4ed6e90b8bfd9517798da357658fec))
* Add support for limiting and reporting costs ([671dafb](https://github.com/aholstenson/kvarn/commit/671dafbdf69b592d38f7d54c896f1b43664deb9e))
* Improve editing tools available to agent ([0171376](https://github.com/aholstenson/kvarn/commit/017137640cf36998d9d96215833bf364cc8336fa))
* Initial commit of proof of concept ([5cdc9fd](https://github.com/aholstenson/kvarn/commit/5cdc9fdd374fc5d1270f097206ebc9a49ad79f5e))
* Introduce support for modes ([01932fb](https://github.com/aholstenson/kvarn/commit/01932fbecf32486e36257d3a89e371c1c90296c0))
* Support for downloading image automatically ([e80e691](https://github.com/aholstenson/kvarn/commit/e80e69112dd157587b2e78119b528da9a8405d16))
* Support thinking mode and controlling output tokens ([00afb1c](https://github.com/aholstenson/kvarn/commit/00afb1cf8a55eaa830f3ee09bd8a361a69637847))


### Bug Fixes

* Raise scanner buffer limit to 1 MB in VM console readers ([046a462](https://github.com/aholstenson/kvarn/commit/046a46210febb75b0fb79aecc7af40345c26819b))
* Reap QEMU process to prevent zombies on unexpected exit ([8b93385](https://github.com/aholstenson/kvarn/commit/8b9338594ab3b262abd4797c8025bd40e48b434b))
* Seed vsock CID counter above in-use CIDs on provider init ([86640c6](https://github.com/aholstenson/kvarn/commit/86640c6d969519d5ff245763c39960a4f48a335b))
* Synchronize Session.Close() to prevent concurrent close data race ([8928e71](https://github.com/aholstenson/kvarn/commit/8928e71ac5a31ab498da8bcaf38f696dfb709b98))


### Miscellaneous Chores

* Bootstrap 0.1.0 release ([d73357b](https://github.com/aholstenson/kvarn/commit/d73357b6dab25207a83618989dbf2cac87eebd2b))
