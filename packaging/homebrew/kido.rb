# Template for the formula in the kido-sh/tap tap (`brew install kido-sh/tap/kido`). For a release,
# set the tag and the sha256 (`shasum -a 256` of the source tarball).
class Kido < Formula
  desc "Docker-like CLI that runs macOS arm64 command-line tools in Darwin micro-VMs"
  homepage "https://kido.sh"
  url "https://github.com/kido-sh/kido/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "Apache-2.0"
  head "https://github.com/kido-sh/kido.git", branch: "main"

  depends_on "go" => :build
  depends_on arch: :arm64
  depends_on macos: :sequoia

  def install
    # `make sign` signs ad-hoc here; release builds are Developer ID signed (packaging/notarize.md).
    # Makefile builds carry no development QEMU path, so kido only finds the QEMU `kido setup` installs.
    system "make", "build", "sign", "VERSION=#{version}"
    system "make", "install", "PREFIX=#{prefix}", "VERSION=#{version}"
  end

  def caveats
    <<~EOS
      kido needs a QEMU built with Hypervisor.framework support and a base image.
      `kido setup` downloads a pinned, checksummed QEMU into ~/.kido/qemu (no sudo):
        kido setup
      and, given a registry reference, also pulls the base image as darwin:latest:
        kido setup --image REGISTRY/darwin:latest
      A base image directory (disk.img, kernel, initrd.cpio, root UUID) imports with:
        kido image import DIR darwin

      Check everything with:
        kido doctor
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/kido version")
    assert_path_exists libexec/"kido/kido-agent"
    system "codesign", "--verify", "--strict", bin/"kido"
    ENV["KIDO_HOME"] = testpath/"kido"
    ENV.delete("KIDO_QEMU")
    assert_match "run `kido setup`", shell_output("#{bin}/kido doctor", 1)
  end
end
