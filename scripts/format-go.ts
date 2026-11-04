import { spawnSync } from "node:child_process";
import { lstatSync } from "node:fs";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";

// Use Git's file list so generated build trees, dependencies, and ignored files
// stay untouched. NUL delimiters also handle spaces and newlines in filenames.
const root = dirname(dirname(fileURLToPath(import.meta.url)));
const args = process.argv.slice(2);
const check = args[0] === "--check";
if (check) args.shift();
if (args[0] === "--") args.shift();
let selected = args;
if (selected.length === 0) {
  const listing = spawnSync(
    "git",
    ["ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", "*.go"],
    { cwd: root, encoding: "utf8" },
  );
  if (listing.error || listing.status !== 0) {
    console.error(listing.error?.message ?? listing.stderr);
    process.exit(1);
  }
  selected = listing.stdout.split("\0").filter(Boolean);
}
const files = [...new Set(selected)].filter((file) => {
  try {
    // Skip tracked deletions and symlinks rather than writing outside the repo.
    return lstatSync(`${root}/${file}`).isFile();
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
    throw error;
  }
});
let needsFormatting = false;
for (let offset = 0; offset < files.length; offset += 100) {
  const result = spawnSync(
    "gofmt",
    [check ? "-l" : "-w", ...files.slice(offset, offset + 100).map((file) => `./${file}`)],
    { cwd: root, encoding: "utf8" },
  );
  if (result.stdout) process.stdout.write(result.stdout);
  if (result.stderr) process.stderr.write(result.stderr);
  if (result.error || result.status !== 0) {
    if (result.error) console.error(result.error.message);
    process.exit(1);
  }
  needsFormatting ||= result.stdout.length > 0;
}
if (check && needsFormatting) {
  console.error("Run make format to format the listed Go files.");
  process.exit(1);
}
