class Rift < Formula
  desc "Tiling window manager for macOS"
  homepage "https://github.com/acsandmann/rift"
  url "https://github.com/acsandmann/rift/releases/download/v0.8.5/rift-app-universal-macos-0.8.5.tar.gz"
  version "0.8.5"
  sha256 "b69933fa15d8f02482c8d3a20aa73d27ad0996a492dcf87401559129bab81c4b"
  license "Apache-2.0"
  head "https://github.com/acsandmann/rift.git", branch: "main"

  def install
    prefix.install "Rift.app"
    bin.install_symlink prefix/"Rift.app/Contents/MacOS/rift"
    bin.install "rift-cli"
    pkgshare.install "rift.default.toml"
  end

  def post_install
    # Reload an already registered Rift LaunchAgent after its executable moves.
    # Leave stopped services stopped; brew services manages its own registration.
    agent = Pathname.new(File.expand_path("~/Library/LaunchAgents/git.acsandmann.rift.plist"))
    if agent.file? && Kernel.system("/bin/launchctl", "print", "gui/#{Process.uid}/git.acsandmann.rift", out: File::NULL, err: File::NULL)
      system opt_bin/"rift", "service", "restart"
    end
  end

  def caveats
      <<~EOS
        Rift requires Accessibility permissions to control windows.
        Grant permissions in System Settings > Privacy & Security > Accessibility.

        To copy the example configuration:
          mkdir -p ~/.config/rift && cp #{pkgshare}/rift.default.toml ~/.config/rift/config.toml
      EOS
  end

  service do
    run "#{opt_prefix}/Rift.app/Contents/MacOS/rift"
    environment_variables PATH: std_service_path_env, LANG: "en_US.UTF-8"
    keep_alive true
    process_type :interactive
    require "etc"
    user = (Etc.getpwuid(Process.uid).name rescue "unknown")
    log_path "/tmp/rift_#{user}.out.log"
    error_log_path "/tmp/rift_#{user}.err.log"
  end

  #test do
  #  system "#{bin}/rift", "--version"
  #end
end
