import { readFileSync } from "node:fs";

const path = new URL("../internal/adapter/web/index.html", import.meta.url);
const html = readFileSync(path, "utf8");
const scripts = [...html.matchAll(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/gi)];
if (scripts.length !== 1) {
  throw new Error(`expected exactly one inline dashboard script, found ${scripts.length}`);
}
// Constructing the function parses the full script without executing DOM code.
new Function(scripts[0][1]);
console.log("Embedded dashboard JavaScript parses successfully.");
