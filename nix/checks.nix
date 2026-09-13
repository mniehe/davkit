{pkgs}: let
  src = ../.;

  # buildGoModule carries its own Go, which nixpkgs still defaults to a release
  # older than this module's go directive. Point it at the overlaid toolchain or
  # every check fails with "go.mod requires go >= 1.27.0".
  buildGoModule = pkgs.buildGoModule.override {go = pkgs.go;};

  # Shared by every check that compiles the module. The hash covers go.sum's
  # closure, so it changes when a dependency does.
  vendorHash = "sha256-s+e6mHjSfV92wH6toCpXV9Cf7SLjKuhxoq5GyT0UFuU=";
in {
  # The Go suite, including conformance/litmus_test.go. litmus is a
  # nativeBuildInput rather than a Go dependency: the test skips without it, so
  # leaving it out here would turn a conformance failure into a silent pass.
  tests = buildGoModule {
    pname = "davkit-tests";
    version = "0.1.0";
    inherit src vendorHash;

    nativeBuildInputs = [pkgs.litmus];

    dontBuild = true;
    doCheck = true;

    checkPhase = ''
      runHook preCheck
      # -race needs cgo, which buildGoModule turns off by default.
      export CGO_ENABLED=1
      # Fail rather than skip if litmus ever drops out of nativeBuildInputs.
      export WEBDAV_REQUIRE_LITMUS=1
      go test -race ./...
      runHook postCheck
    '';

    installPhase = ''
      mkdir -p $out
      echo "tests passed" > $out/result
    '';
  };

  lint = buildGoModule {
    pname = "davkit-lint";
    version = "0.1.0";
    inherit src vendorHash;

    nativeBuildInputs = [pkgs.golangci-lint];

    dontBuild = true;
    doCheck = true;

    checkPhase = ''
      runHook preCheck
      export GOLANGCI_LINT_CACHE=$TMPDIR/golangci-cache
      golangci-lint run ./...
      runHook postCheck
    '';

    installPhase = ''
      mkdir -p $out
      echo "lint clean" > $out/result
    '';
  };

  formatting =
    pkgs.runCommand "check-formatting" {
      nativeBuildInputs = [pkgs.go];
    } ''
      cd ${src}
      export GOCACHE=$TMPDIR/go-cache
      unformatted=$(gofmt -l .)
      if [ -n "$unformatted" ]; then
        echo "gofmt needed on:" >&2
        echo "$unformatted" >&2
        exit 1
      fi
      mkdir -p $out
    '';
}
