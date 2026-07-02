# Homebrew formula for Live Git (lg).
#
# To let people `brew install` lg, put this file in a tap repo named
# `homebrew-livegit` (e.g. github.com/iamtaehyunpark/homebrew-livegit), then:
#
#   brew tap iamtaehyunpark/livegit
#   brew install lg
#
# Update `version`, `url`, and the `sha256` values for each release
# (sha256 of each dist/lg-<os>-<arch> binary). Homebrew downloads the prebuilt
# binary — users never need Go.
class Lg < Formula
  desc "Live Git — real-time codebase sync + remote execution (Ghost <-> Source)"
  homepage "https://github.com/iamtaehyunpark/livegit"
  version "1.0.0"

  on_macos do
    on_arm do
      url "https://github.com/iamtaehyunpark/livegit/releases/download/v1.0.0/lg-darwin-arm64"
      sha256 "f043b393455ab49e78370ecb5553f878e5fe1436c4a99e4ad80c57890728b977"
    end
    on_intel do
      url "https://github.com/iamtaehyunpark/livegit/releases/download/v1.0.0/lg-darwin-amd64"
      sha256 "ebc47daa7ffa01f33620734cf62ef4dfb69cebcf068136d09ed5380688a60261"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/iamtaehyunpark/livegit/releases/download/v1.0.0/lg-linux-arm64"
      sha256 "80e2c2faad6b5e49eb3b51af268b529221e78e0732e13effd1a8b9ed8e9eb013"
    end
    on_intel do
      url "https://github.com/iamtaehyunpark/livegit/releases/download/v1.0.0/lg-linux-amd64"
      sha256 "94a3b77c5e675301cffd415051e7b87a9e11b6199120c2a17b8dc0b4263a3c12"
    end
  end

  def install
    bin.install Dir["*"].first => "lg"
  end

  test do
    assert_match "lg", shell_output("#{bin}/lg --version")
  end
end
