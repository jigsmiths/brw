cask "bento" do
  version "0.1.4"
  sha256 "84ff1a54df1005802bfc1ec53be006c8cfcf11024dca526f2419f679891b7888"

  url "https://github.com/ThreeDotsLabs/homebrew-tap/releases/download/v#{version}/Bento-#{version}-arm64.zip"
  name "Bento"
  desc "Run coding agents side by side"
  homepage "https://github.com/ThreeDotsLabs/homebrew-tap"

  depends_on arch: :arm64
  depends_on macos: :monterey

  app "Bento.app"
  binary "#{appdir}/Bento.app/Contents/MacOS/bento"

  # No auto_updates: bento's Update button runs brew upgrade, which
  # skips casks that say they update themselves.
  # TODO: sign with a Developer ID and notarize, then drop this step.
  # The app is not notarized, so Gatekeeper would refuse the download.
  postflight_steps do
    run "/usr/bin/xattr", args: ["-rd", "com.apple.quarantine", "{{appdir}}/Bento.app"]
  end

  zap trash: "~/Library/Logs/bento"

  caveats <<~EOS
    The Private VM sandbox needs Lima: brew install lima
  EOS
end
