# Changelog

## [0.5.0](https://github.com/aholstenson/kvarn/compare/v0.4.0...v0.5.0) (2026-08-10)


### Features

* Allow VM images up to the 0.4 line ([5fd9b6d](https://github.com/aholstenson/kvarn/commit/5fd9b6d647b652043de5143315df239e1e8c4c8c))
* **image:** Include tun in the VM image ([4019074](https://github.com/aholstenson/kvarn/commit/401907405ed6326c2a47e9c873c1721a63a1c3dd))
* Improve network allowlist and reporting of denied requests ([6ecd8bf](https://github.com/aholstenson/kvarn/commit/6ecd8bf81304505d2177f269d615ba21b9b717f0))
* Improve support for tools like Mise ([5ce16e1](https://github.com/aholstenson/kvarn/commit/5ce16e1e05d30765fd7f5081c29dc20c6d9b1346))
* Support hiding work log on PRs ([0d17a40](https://github.com/aholstenson/kvarn/commit/0d17a400fc0cdec408a216dd63241698b80dd2ad))
* Update llms-go to version supporting latest Claude models ([96a6bb7](https://github.com/aholstenson/kvarn/commit/96a6bb7e5cd3df86753dbe0f67e536a7c33138e1))


### Bug Fixes

* Move certificate management into orchestrator instead of cloud-init ([100a374](https://github.com/aholstenson/kvarn/commit/100a374096598fc02df3935f07335273a5540047))
* Submit changes the agent committed inside the VM instead of reporting "no changes" ([917e11e](https://github.com/aholstenson/kvarn/commit/917e11ece1e35e19607395b4d7d88808b1749bd3))

## [0.4.0](https://github.com/aholstenson/kvarn/compare/v0.3.0...v0.4.0) (2026-08-09)


### Features

* Allow projects to define custom modes via kvarn.yml ([eabb964](https://github.com/aholstenson/kvarn/commit/eabb96488f3ef3bae618c79e41b3c22e6395465a))
* Let each sub-agent pick a model class instead of sharing one setting ([93847b2](https://github.com/aholstenson/kvarn/commit/93847b20025aa299133a6ea7bc8acc6e71953491))
* Let job submissions carry caller metadata for record keeping ([ebe2184](https://github.com/aholstenson/kvarn/commit/ebe21840aeed9f5ae1b206696f6320b84ca441a4))

## [0.3.0](https://github.com/aholstenson/kvarn/compare/v0.2.0...v0.3.0) (2026-08-04)


### Features

* Ability to store LLM credentials in TOML config ([1db80fd](https://github.com/aholstenson/kvarn/commit/1db80fd1ef967139c0e6e1efa23aafc17bb3d32e))
* Add ability to control clone depth ([46d304f](https://github.com/aholstenson/kvarn/commit/46d304f77ba64918f7652702389c551f9a42f69c))
* Add built-in hostkeys for Git cloning ([8b2b1a7](https://github.com/aholstenson/kvarn/commit/8b2b1a7c2a3266cbc99b0ae4fe5c49175f7cb43b))
* Add cache for container images ([b80c25a](https://github.com/aholstenson/kvarn/commit/b80c25a3b6711305d7b47b9600a4a5fa5babd10a))
* Add improved job control via RPC and CLI ([0a96a7f](https://github.com/aholstenson/kvarn/commit/0a96a7f8177db06fae69b1cc65c08eea8beeb58c))
* Add local host-control socket and admin capabilities for keys ([e263ac0](https://github.com/aholstenson/kvarn/commit/e263ac0927673f2368c577b878211e15de28edea))
* Add persistent storage for sessions ([b3d7fea](https://github.com/aholstenson/kvarn/commit/b3d7fea45b3d18558ea234fbfeb5409e19e51643))
* Backfill past jobs that don't fit, bounded by aging ([2150d58](https://github.com/aholstenson/kvarn/commit/2150d58627e324197eb1a4066a2dc304e8dbea01))
* Boot VMs from a qcow2 overlay instead of copying the base image ([dbec9cb](https://github.com/aholstenson/kvarn/commit/dbec9cb940e23bd8adb3b5a88dcf140fed46462a))
* Clone full Git repositories for faster jobs ([3e545a2](https://github.com/aholstenson/kvarn/commit/3e545a2bb86ef438b62e309c4e4406e2596ed42e))
* Fix some CLI flags to make them more logical ([d8e8d6b](https://github.com/aholstenson/kvarn/commit/d8e8d6beb0d465bada14194c7207ee454fadc406))
* Idempotency support on job starts ([a8e4ecb](https://github.com/aholstenson/kvarn/commit/a8e4ecba34663de0970a215b0b182083e150fcfa))
* Improve prompt to encourage verification and thinking ([1a3c602](https://github.com/aholstenson/kvarn/commit/1a3c602b4ddf120b3dc34466a4e22dc588c1e228))
* Improve secret injection supporting bearer, basic and OAuth schemes ([9fcaa18](https://github.com/aholstenson/kvarn/commit/9fcaa183147738e459ec106fd767cc5eda4c5f0e))
* Improve VM sweeping and cleanup ([6a50c28](https://github.com/aholstenson/kvarn/commit/6a50c28ecef356e297d1b7cb85710085a38283fc))
* Install Nix 2.34.7 in the image ([b4992c5](https://github.com/aholstenson/kvarn/commit/b4992c57d5b743a2f2728c0db1c9f47780bd2851))
* Job queue can now be drained/resumed ([87629e2](https://github.com/aholstenson/kvarn/commit/87629e29c7baf7ba4509aa802a045c1a7942a2f3))
* Make plan optional in implement mode ([7d19485](https://github.com/aholstenson/kvarn/commit/7d19485d2df98b69fc935a7b0a4a2561f3cf0b36))
* Move startjob and cancel into jobs subcommand ([3b8e70f](https://github.com/aholstenson/kvarn/commit/3b8e70faa5a27ba6f9da189fdf1e387a11ec12fb))
* Order the queue by fair share and aging priority ([be30da8](https://github.com/aholstenson/kvarn/commit/be30da82fbc14c064689029ea4a8622a509392bd))
* Submitted jobs are now durable and survive restarts ([cdc7c31](https://github.com/aholstenson/kvarn/commit/cdc7c31c6fb4e33f63e3f435e9c23f9dad94bc04))
* Support bounding the job queue ([05dc7ed](https://github.com/aholstenson/kvarn/commit/05dc7ed946afbe21eff143bdbb7988be55a2fc5e))
* Support cancelling a running job ([fc0b5d3](https://github.com/aholstenson/kvarn/commit/fc0b5d3b3e6ece181a51a32f30ab9346e44189e0))
* Support capping concurrent jobs per project and per API key ([45e6fc8](https://github.com/aholstenson/kvarn/commit/45e6fc86d7e66102252213e991eaf7ac0cb3735b))
* Support for feedback runs on existing PRs ([d7d8110](https://github.com/aholstenson/kvarn/commit/d7d8110f75cf509ed5fb3ecbe593c4eb947eb693))
* Support overcommiting thin VM disks ([01fe5b0](https://github.com/aholstenson/kvarn/commit/01fe5b0574bcc6bf5eebda08b3de4e31e73fab20))
* Support relative cache paths ([d172431](https://github.com/aholstenson/kvarn/commit/d1724315f120e5e2ea479862061a4337b0890136))


### Bug Fixes

* Don't silently drop errors from Github ([9583eb1](https://github.com/aholstenson/kvarn/commit/9583eb1a35f491a3dc87b58340fbc053476817e8))
* Fail sessions when push or PR creation fails ([ddcf4b7](https://github.com/aholstenson/kvarn/commit/ddcf4b73841c6aa642f84d0aa99043831a6533e8))
* Gracefully handle panics during sandbox close ([db1a77f](https://github.com/aholstenson/kvarn/commit/db1a77fccdc58b200cc9a65b93b1f33ad948cdba))
* macOS hosts can now read the memory size properly ([7b9c316](https://github.com/aholstenson/kvarn/commit/7b9c316b801aa0a12149c2f68d634bfbf32b0730))
* Preserve file modes and symlinks when extracting VM changes ([6c1adca](https://github.com/aholstenson/kvarn/commit/6c1adcab90ff210a10f9ff6426144bd4ef2973c2))
* Re-resolve forge credentials at push time ([458b2db](https://github.com/aholstenson/kvarn/commit/458b2dbb45ee4f1977d4940253f05cf71f900379))
* Show full line-by-line output in run and test commands ([abed35a](https://github.com/aholstenson/kvarn/commit/abed35aec0ca71fe548b4d957a78a4a3402e3218))
* Write terminal session state on an uncancellable context ([54c0b57](https://github.com/aholstenson/kvarn/commit/54c0b57bcf98df442aa57fe9a65e61705430bf7c))

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
