"""Pinned OpenTofu release metadata.

Every stable OpenTofu release from the 1.12.0 floor upward is pinned here and
kept indefinitely. `.github/workflows/tofu_versions.yml` pins new releases
weekly; to pin one or more releases by hand, run
`bazel run //tools:update_versions -- <version>...`, or pass `--default` to set
`DEFAULT_VERSION` to the newest version given. The script verifies the cosign
signature on each release's `SHA256SUMS` before writing here.

`rules_tofu` downloads OpenTofu release binaries by design rather than building
them from source. The pinned download is already hermetic: content-addressed,
cached by Bazel and offline after the first fetch. A source build would pull
roughly 300 Go modules into every downstream module graph through `go_deps`, and
as a non-root module `rules_tofu` could not apply OpenTofu's `replace` of
`hashicorp/hcl`. A root module that wants a source build can register its own
toolchain; see "The `tofu` binary" in `README.md`.
"""

# Follows the newest stable OpenTofu release. OpenTofu 1.12 is the oldest
# release rules_tofu supports because the rules pass flags that older releases
# lack, such as `tofu test -json-into` from 1.12.0; pin nothing older here.
DEFAULT_VERSION = "1.13.9"

# Version -> { "<os>_<arch>": "<sha256>" }, newest first. Keys match the
# <os>_<arch> suffix of OpenTofu's release archives,
# tofu_<version>_<os>_<arch>.zip; PLATFORMS maps each key to Bazel's OS and CPU
# constraints.
KNOWN_VERSIONS = {
    "1.13.9": {
        "linux_amd64": "bc44eb6b06446ba83bd4072114891987f590f66cca712737a7f1147c50c539ce",
        "linux_arm64": "04f7c93828b859d420269ed30f923ffebf09157b5d7fb95bf8d88a4d208b7fa3",
        "darwin_amd64": "ac5505dd19d896f327d0ab727d4b853413c694931a7602b31f93c3a3e9d46d00",
        "darwin_arm64": "29ba2f4c78556a8d25476e5070fe4745495ef649c4a3b827170eb7436d23feb3",
        "windows_amd64": "b23a811741911baa829aa7d3d3718626c549e8934546822b0e9b99b9fba75e4e",
    },
    "1.13.8": {
        "linux_amd64": "aa4382a5eefcf026309d07398ea803f97b9218a17d940999a938be22bfe9d73f",
        "linux_arm64": "efb05ff0e2403c8c29a6c7a02d5d95bf86f987063d1bbd5ae3e47621f752d139",
        "darwin_amd64": "325e929c2ac4a8fb2077a7d52ad9a3fa8c012a745a5e5da0bf3fdbbeb99a3fad",
        "darwin_arm64": "7cd554aa1dd196ea87d5b009edeed0138ef2e21ff8154f829ef71b4f7421806d",
        "windows_amd64": "8f01d1b523e770e0363207ec463b2fdb70f437405b261321533c8961f65297ba",
    },
    "1.13.2": {
        "linux_amd64": "723bebb0df8411daff1e5fc3862e28ff5581fe5634087f4c29a9aa41d78208b5",
        "linux_arm64": "6e815c58a09ee957de606ff48cefad082942e70afc43b737ff975654c54ec453",
        "darwin_amd64": "2093d0f881d911eb133cf51d2d7e09f22da71b1e6465c60826a314d217b0b84e",
        "darwin_arm64": "cb46112c095f1c4da721b6a18c8368f67341896a62205d1c117299e85993af61",
        "windows_amd64": "88b5cbb12e3a7402b0b1fe148790477fb6193cd8201864fa7fb0bd8e6417ee0e",
    },
    "1.13.1": {
        "linux_amd64": "8ccbc6f8ee21d2827715f3c6e08a9b3e0209b1e62057c05067ef117e047c1a80",
        "linux_arm64": "b9614df40575cc3fc10a8a25025b7245d961da279f715ea3efff4ddae8e6938a",
        "darwin_amd64": "d44ab59ec53fb18e900f4ae3fc8b72e042c9e03ca957860edbaac04e4427218d",
        "darwin_arm64": "81aebe6453223bcb3ce3c28424b36d009ebfddd3ad65ad1ec3eed0d0d574a77b",
        "windows_amd64": "5b653d1d95719eeec4ccf55f0e2102081b6f589d788197f1cabeb89cbd4078a7",
    },
    "1.13.0": {
        "linux_amd64": "ad494034a03aaa66d93fc1c2c164d01bedf21b44cfb8b616182cb69424a67672",
        "linux_arm64": "34dfd5c6d7de0372789d92dc0db52a9c710edac7cc2abef404a76a1fae6f2ef8",
        "darwin_amd64": "c4eea55d524073fc90244a035fb2c31a3f108bcedf16e7f449f0831e772d3287",
        "darwin_arm64": "6e03dc4d12df57c0f7da38cb963f28e8d0418eb6f0762a2fc5eb3988da6b396d",
        "windows_amd64": "cc19de2b9461d62ccf73d422703d939c44d6ffcd94e95bacf43752c34125f5d6",
    },
    "1.12.12": {
        "linux_amd64": "723df4d3044ca2e1fd8469d715a24869889c53e7f45458b1f2ea2e40e344789d",
        "linux_arm64": "77d73e8cc262d6d065779741e6d429abbfa587db6fd50184ba36f7c154d8595b",
        "darwin_amd64": "324787bd0877be33fdb8d3cb7a02d88a598fc965b8be85d6c5c879d8a5a40b97",
        "darwin_arm64": "8fe0aade49b58c35061035a296bab2ec52eb66309482bb193f5d6db37e3b5d3e",
        "windows_amd64": "237c29f25bee7e6606ab07fd7c09c6ae90b41466b0d9e4bdd53e499c627d7b96",
    },
    "1.12.11": {
        "linux_amd64": "590cab0e84b44c27b1ee022d80c6bb32a70a0ad46c50ff59eb16a552bf86b54c",
        "linux_arm64": "0bd677188f1cf1bc11a52a4ad6eb676a738cd856f0e8a25ac7de102c61488f14",
        "darwin_amd64": "b7d53ad8d60a57dd741bcc92f6b26efee97f39faa86cd6237467d83b2635103e",
        "darwin_arm64": "ea3590fae02a0aed19313f75b0d04daac9721be290b4e220109c90e9ff6f4d06",
        "windows_amd64": "e568066584decfc67886bc97493acacb62c26ffcf81a62a7d7db17ece519ba49",
    },
    "1.12.9": {
        "linux_amd64": "da7902a32837c2c8a0a5f999c4fbdc622ef2faedbc1755d6841bf2353a5079c0",
        "linux_arm64": "454b25e0675f13da0b794dfcc562a16f64cb997b2491d5102170102aec701d4f",
        "darwin_amd64": "2fb316a38f75edb78e282b9a83d2010afedb1f5bd5c29666ab8806ea9ffee353",
        "darwin_arm64": "5029ca95f76fe1f9bebee9be8082698ec63651a1b8a5d4a2f17bf100e30610b0",
        "windows_amd64": "c05bdcfef00c91e38f92b27530fb146016679835193c8171fc48758a4a2e322d",
    },
    "1.12.8": {
        "linux_amd64": "e280accc5e074c69d6b731cca939d4d030fdffc616b146e35227cd0c9208ba82",
        "linux_arm64": "255c39923b2e1ad969fbdb6460e43cbbb90156b58d586fa6f361c2acd6403afb",
        "darwin_amd64": "fe3a4fda983bc6919ba80a31ca4ea123cbaae18283c28515e36d512f66b59458",
        "darwin_arm64": "b3974ac6b17b2e283a7c3e5dace22d9b70b0d96b4f0d3714b6ed04153694e53b",
        "windows_amd64": "10bbdf1856184dc18e05090692e0a67c788e532cc6ee593bcf12902fc873b7cd",
    },
    "1.12.7": {
        "linux_amd64": "6807f7ae2a33e254c104da54e628b40aa6fc50d4becd78c43574fe250add33e9",
        "linux_arm64": "a63fc6bfef855c5362c969808253d526abb308a9dc625c0f192f3adbe5d364cd",
        "darwin_amd64": "9c2f988aa5de30b6ed8d3bca7c4f95ae4c699ac899cbbc3fbe46fc3d51e3fbb7",
        "darwin_arm64": "e3a30269d64b3b9f49937e835817e2c06871fc4cfb7a08622d3b752074f75fb8",
        "windows_amd64": "f9a7a9a95bce4b73b792335a9aa9e68219975c294e013f2a02238149e0010be5",
    },
    "1.12.6": {
        "linux_amd64": "5dc43da4f750f33873dc25e94587128709e819e544b7be9016b255316153c3a8",
        "linux_arm64": "e573979ba68a17fe7b881752051a694a7efcd970e39521f6a25775197861ed4d",
        "darwin_amd64": "166388e5feed47e107e11721b6366bf91d21e47eccbced75f3cbe0c7184ffd9b",
        "darwin_arm64": "e083ee43790ab9e19ad66d9933e24a7244a1412e1d5728f37999ae2163fdac95",
        "windows_amd64": "0d1421721cf9ec24b41b698a9620dda218d47fa7e76ac3dc15cdbc13bd79b0bb",
    },
    "1.12.5": {
        "linux_amd64": "dade9650e6b74fc7a8b986bd8717497d32f9e09cf82e479afef4977fa3085536",
        "linux_arm64": "528f4eea63452bbddb30fa4f1780b57fac8d7676f9dda0f772e847bb62c1260a",
        "darwin_amd64": "45ab896c37c9e2b461604d3fd162867e825bef85b2c2c4c5443b28dc5fb51bd4",
        "darwin_arm64": "dbb5a5bae9b0cabf622cd81a80ea02230eae8a3813215400df41a2cb89b47157",
        "windows_amd64": "465adc4bdf4aa80c650387b7ada2cf750f7908bf1fae7f35b05402c384e79928",
    },
    "1.12.4": {
        "linux_amd64": "f5d2ae8a0efcddd3722546b3e0f2f2f0648ce5e5a3e411176041adcb7dccc1e8",
        "linux_arm64": "a3b01db857c7c650768ffa8ad9119dc2db82fe1b98125b7238392a160aca7f8a",
        "darwin_amd64": "ff4d49559157697b4e3651868aead7ce0e85744242e1b60679f29d6ddc777a45",
        "darwin_arm64": "e5e8db9c2dd2c657a8b46931e41cd8dd1d89e5a30aebd742f4f8eafcf1815a35",
        "windows_amd64": "16a8d272e368c8e4fdb5c665fb9dbb4bc89b505cf32895bc4d81c21dfd6beda9",
    },
    "1.12.3": {
        "linux_amd64": "46b48c3438c65cf479fc076c9281422ffa2f493548d1e813d154c835c5986a08",
        "linux_arm64": "b2110d1ce46e366ce861b7f53d293dad99080075629aed7fb50d7328916d91c2",
        "darwin_amd64": "0898350dcc5b2ae31ad104cf4882228d08f858ba28f4e8bea693b51d1b267c57",
        "darwin_arm64": "2b81c065cdcf5e573cfb5d9e0c663ac4cfc32512927078b645b58ef81cec2474",
        "windows_amd64": "92d2087f0ba8e676e75d940c20b5359f191c518e2916d4e77f9d1fe38bbd3f52",
    },
    "1.12.2": {
        "linux_amd64": "dc0ad16a42b3bfb5f3ceff3dcd5e9cd4c55271fae9f4bfe611749ff8ae6ec23c",
        "linux_arm64": "360a2f238c100818851917531f1ead80c1bda960f9f38f965820fa85fd5147a7",
        "darwin_amd64": "1027862fc1c098a5307a5addc9a6be33b9bb79e9b4c8aace6b65132def9d5de3",
        "darwin_arm64": "234d419b13e4c50bb75c61d6349b31e7f7dde407fb5317e9063fd00d3bf15646",
        "windows_amd64": "9c9c33b6a92b8d8c4850dd562df199767c825bff4c08f0955a59bad0c5d5ffaf",
    },
    "1.12.1": {
        "linux_amd64": "1fc9af962e3632b7cd0ba27076cd9f1ced177567defe9e331ac37f5a40468575",
        "linux_arm64": "1a53dd57697dc04d243ddb81a0f70e44ab83c256f15dde173e5538120dc6a0bb",
        "darwin_amd64": "bc1bf25272976a568831889b42c4dddad5897c348e5ecc63960a732feee8a73f",
        "darwin_arm64": "06cff265b39c437ba81f91cfa85ae91b6ae28d3112bb2273294431a3f54e8bf5",
        "windows_amd64": "a6d8fd924753ab6d3d4f0143d29214a31679a5fca8bfb59f5edde263b3a1c3fc",
    },
    "1.12.0": {
        "linux_amd64": "8d7650fd42b6d790f9f747604393ccd0a9035376bccc4f1688b905d7c5bb1137",
        "linux_arm64": "466bf912404b4ab0f0b3a043073d68ad34f11d55ad7a483957d94f0733169f8d",
        "darwin_amd64": "761dc6688325721be230f95b94382bc06ffe59d87cb25c94ef8a37d9cb0c0014",
        "darwin_arm64": "1b09890dc4ed842bebb55b8c958943b28bc025b3728ee2e5f848c30ee3406841",
        "windows_amd64": "7253abf6ce9c0e88e0cc188c5c883e02353b6c5ffcf2125e6c307348ca223df0",
    },
}

# (platform_key, bazel_os_constraint, bazel_cpu_constraint, exe_suffix)
PLATFORMS = [
    ("linux_amd64", "@platforms//os:linux", "@platforms//cpu:x86_64", ""),
    ("linux_arm64", "@platforms//os:linux", "@platforms//cpu:aarch64", ""),
    ("darwin_amd64", "@platforms//os:macos", "@platforms//cpu:x86_64", ""),
    ("darwin_arm64", "@platforms//os:macos", "@platforms//cpu:aarch64", ""),
    ("windows_amd64", "@platforms//os:windows", "@platforms//cpu:x86_64", ".exe"),
]
