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
  version "0.2.0"

  on_macos do
    on_arm do
      url "https://github.com/iamtaehyunpark/livegit/releases/download/v0.2.0/lg-darwin-arm64"
      sha256 "REPLACE_WITH_SHA256_OF_lg-darwin-arm64"
    end
    on_intel do
      url "https://github.com/iamtaehyunpark/livegit/releases/download/v0.2.0/lg-darwin-amd64"
      sha256 "REPLACE_WITH_SHA256_OF_lg-darwin-amd64"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/iamtaehyunpark/livegit/releases/download/v0.2.0/lg-linux-arm64"
      sha256 "REPLACE_WITH_SHA256_OF_lg-linux-arm64"
    end
    on_intel do
      url "https://github.com/iamtaehyunpark/livegit/releases/download/v0.2.0/lg-linux-amd64"
      sha256 "REPLACE_WITH_SHA256_OF_lg-linux-amd64"
    end
  end

  def install
    bin.install Dir["*"].first => "lg"
  end

  test do
    assert_match "lg", shell_output("#{bin}/lg --version")
  end
end
