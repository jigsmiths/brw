class Menuanywhere < Formula
  desc "the macos menu bar at your fingertips"
  homepage "https://github.com/acsandmann/menuanywhere"
  url "https://api.github.com/repos/acsandmann/menuanywhere/tarball/1.0.6"
  sha256 "9a07bf73bc119be19f809a6e85434e3af66572d1a416418958d0a6f3e6927d7b"
  version "1.0.6"
  license "MIT"

  depends_on xcode: ["12.0", :build]
  depends_on :macos

  def install
    system "make", "menuanywhere"
    bin.install "menuanywhere"
  end

  service do
    run [opt_bin/"menuanywhere"]
    keep_alive true
    log_path var/"log/menuanywhere.log"
    error_log_path var/"log/menuanywhere.error.log"
    working_dir HOMEBREW_PREFIX
  end

  test do
    assert_predicate bin/"menuanywhere", :exist?
    assert_predicate bin/"menuanywhere", :executable?
  end
end
