# Changelog

## [0.3.0](https://github.com/aholstenson/kvarn/compare/v0.2.0...v0.3.0) (2026-06-07)


### Features

* Add ability to control clone depth ([46d304f](https://github.com/aholstenson/kvarn/commit/46d304f77ba64918f7652702389c551f9a42f69c))
* Add built-in hostkeys for Git cloning ([8b2b1a7](https://github.com/aholstenson/kvarn/commit/8b2b1a7c2a3266cbc99b0ae4fe5c49175f7cb43b))
* Add cache for container images ([b80c25a](https://github.com/aholstenson/kvarn/commit/b80c25a3b6711305d7b47b9600a4a5fa5babd10a))
* Add persistent storage for sessions ([b3d7fea](https://github.com/aholstenson/kvarn/commit/b3d7fea45b3d18558ea234fbfeb5409e19e51643))
* Improve secret injection supporting bearer, basic and OAuth schemes ([9fcaa18](https://github.com/aholstenson/kvarn/commit/9fcaa183147738e459ec106fd767cc5eda4c5f0e))
* Install Nix 2.34.7 in the image ([b4992c5](https://github.com/aholstenson/kvarn/commit/b4992c57d5b743a2f2728c0db1c9f47780bd2851))
* Support relative cache paths ([d172431](https://github.com/aholstenson/kvarn/commit/d1724315f120e5e2ea479862061a4337b0890136))


### Bug Fixes

* Don't silently drop errors from Github ([9583eb1](https://github.com/aholstenson/kvarn/commit/9583eb1a35f491a3dc87b58340fbc053476817e8))
* Gracefully handle panics during sandbox close ([db1a77f](https://github.com/aholstenson/kvarn/commit/db1a77fccdc58b200cc9a65b93b1f33ad948cdba))
* Show full line-by-line output in run and test commands ([abed35a](https://github.com/aholstenson/kvarn/commit/abed35aec0ca71fe548b4d957a78a4a3402e3218))

## [0.2.0](https://github.com/aholstenson/kvarn/compare/v0.1.0...v0.2.0) (2026-05-31)


### Features

* Ability to limit number of jobs executing based on their resource usage ([224035f](https://github.com/aholstenson/kvarn/commit/224035fcf4b831ce228133b9be5205780038f1dd))
* Add improved operating instructions for agent ([4545b54](https://github.com/aholstenson/kvarn/commit/4545b5495d69128e336da34cedeb7a38e8055f63))
* Add observability metrics and improve logging ([e87baa8](https://github.com/aholstenson/kvarn/commit/e87baa886424bda08f292e8533085f8aeca5bce3))
* Clean up orphaned VMs and limit their runtime ([261a7c5](https://github.com/aholstenson/kvarn/commit/261a7c5e960e83f00de634cd22bf08ce47ff61cb))
* Graceful shutdown of orchestrator ([ad08eea](https://github.com/aholstenson/kvarn/commit/ad08eea934a42a2468219509f145a6e23131b92d))
* Harden runner bridge against in-VM impersonation ([f3cb040](https://github.com/aholstenson/kvarn/commit/f3cb040216a832001c3b3123781c8797c096dc75))
* Improve caching layer to support lockfiles ([5d34c3e](https://github.com/aholstenson/kvarn/commit/5d34c3e2acaff8eb725c9385825e0448f012dc49))
* Improve UI for local commands ([6d00fe7](https://github.com/aholstenson/kvarn/commit/6d00fe7e42a319a0903d19f8ff107114af4f4e38))
* Protect against multiple processes changing the same file ([a91275f](https://github.com/aholstenson/kvarn/commit/a91275f9d98dbb1488cbe592d9d95960b485a78a))
* Support for per-project overrides of branch and commit info ([10e471f](https://github.com/aholstenson/kvarn/commit/10e471f043b2386d548405c36b6dd9e3eefb2e3a))
* Use commit title to create branch name ([103a639](https://github.com/aholstenson/kvarn/commit/103a639d83ce890ab4188957bed9a7bc4ec98296))
* Use reasoning effort instead of thinking tokens ([d3ce214](https://github.com/aholstenson/kvarn/commit/d3ce21414d5bb52f7af6159cc7ed661aec50d34f))
* When validation step fails ask agent to fix it ([ad2ef7e](https://github.com/aholstenson/kvarn/commit/ad2ef7e83a0ff998e25e0e816259b7e0d90d9371))


### Bug Fixes

* Avoid leaking auth rejected details over API ([d4b5f96](https://github.com/aholstenson/kvarn/commit/d4b5f961528c4a459c2dd6078e537b03c4aaaa55))

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
