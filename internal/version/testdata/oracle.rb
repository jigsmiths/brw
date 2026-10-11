# Regenerates compare.json from Homebrew's own Version class:
#   brew ruby internal/version/testdata/oracle.rb formula.json > internal/version/testdata/compare.json
require "json"

api = JSON.parse(File.read(ARGV.fetch(0)))
versions = api.map { |f| f.dig("versions", "stable") }.compact
versions += Dir["#{HOMEBREW_CELLAR}/*/*"].map { |p| File.basename(p).sub(/_\d+\z/, "") }
edge = %w[1.0 1.0.0 1.0a 1.0alpha 1.0alpha2 1.0b1 1.0beta 1.0pre1 1.0rc 1.0rc1 1.0rc2 1.0p1 1.0-p2
               1.0.post1 1.0.post2 1.0.1 1.0.10 1.01 1.1 01 0 00 1 10 2 2.0 20240203-110809 2026.10.51501
               HEAD HEAD-abc123 1.0xpost1 1.0patch1 1.0-1 1.0_1 v1.0 r15b01 1.0.0.0 1.0a1b2]
versions = (versions + edge).uniq
srand(1)
pairs = versions.flat_map { |a| Array.new(3) { [a, versions.sample] } }.sample(1200)
pairs += edge.product(edge)
out = pairs.uniq.map { |a, b| { a: a, b: b, cmp: Version.new(a) <=> Version.new(b) } }
puts "[", out.map { |o| "  #{o.to_json}" }.join(",\n"), "]"
