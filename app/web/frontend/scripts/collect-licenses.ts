// Collects the license texts of the packages the static export can contain:
// the `dependencies` of package.json and their own dependencies, resolved the
// way Node resolves them. Build-only tooling is in devDependencies and
// platform binaries are optionalDependencies; neither ships in the export.
//
//   bun scripts/collect-licenses.ts OUT_DIR
//
// OUT_DIR/JS-PACKAGES.txt lists `name version license file` per package, and
// each text is copied unmodified to OUT_DIR/js/<name>/<file>. A package that
// ships no license file is listed with file `-` and its declared license.
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { z } from "zod";

const manifest = z.object({
  name: z.string(),
  version: z.string(),
  license: z.string().optional(),
  dependencies: z.record(z.string(), z.string()).optional(),
});

const out = process.argv[2];
if (!out) {
  console.error("usage: collect-licenses.ts OUT_DIR");
  process.exit(2);
}
const root = path.resolve(import.meta.dir, "..");

function resolvePackage(name: string, from: string): string {
  for (let dir = from; ; dir = path.dirname(dir)) {
    const candidate = path.join(dir, "node_modules", name);
    if (existsSync(path.join(candidate, "package.json"))) {
      return candidate;
    }
    if (path.dirname(dir) === dir) {
      throw new Error(
        `collect-licenses: ${name} is not installed (from ${from})`,
      );
    }
  }
}

// Installed copies are visited once per directory; the output lists each
// name@version once even when the install tree holds it twice.
const visited = new Set<string>();
const packages = new Map<
  string,
  { dir: string; pkg: z.infer<typeof manifest> }
>();
const pending = Object.keys(
  manifest.parse(
    JSON.parse(readFileSync(path.join(root, "package.json"), "utf8")),
  ).dependencies ?? {},
).map((name) => ({ name, from: root }));
for (let next = pending.pop(); next; next = pending.pop()) {
  const dir = resolvePackage(next.name, next.from);
  if (visited.has(dir)) {
    continue;
  }
  visited.add(dir);
  const pkg = manifest.parse(
    JSON.parse(readFileSync(path.join(dir, "package.json"), "utf8")),
  );
  packages.set(`${pkg.name}@${pkg.version}`, { dir, pkg });
  for (const name of Object.keys(pkg.dependencies ?? {})) {
    pending.push({ name, from: dir });
  }
}

mkdirSync(path.join(out, "js"), { recursive: true });
const lines: string[] = [];
for (const [key, { dir, pkg }] of [...packages].sort(([a], [b]) =>
  a.localeCompare(b),
)) {
  const file = readdirSync(dir)
    .filter((entry) => /^(licen[cs]e|copying)/i.test(entry))
    .sort()[0];
  if (file) {
    const target = path.join(out, "js", key);
    mkdirSync(target, { recursive: true });
    copyFileSync(path.join(dir, file), path.join(target, file));
  }
  lines.push(
    `${pkg.name} ${pkg.version} ${pkg.license ?? "UNKNOWN"} ${file ?? "-"}`,
  );
}
writeFileSync(path.join(out, "JS-PACKAGES.txt"), `${lines.join("\n")}\n`);
console.log(`collect-licenses: ${lines.length} packages`);
