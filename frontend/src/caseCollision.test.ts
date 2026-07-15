import { describe, it, expect } from "vitest";

// Case-portability guard (added after a broken deploy build slipped past a green
// Linux CI): two source modules whose paths collide once case + extension are folded
// — e.g. `Onboarding.tsx` (component) vs `onboarding.ts` (engine) — resolve fine on a
// case-sensitive filesystem but break the bundler's module resolver on a
// case-INsensitive one (macOS / Windows), where `import x from "./Onboarding"` can't
// be disambiguated from `./onboarding`. Linux CI can never catch this by construction,
// so this test does — in our own gate, on every OS.
//
// Key = directory + basename, extension stripped, lowercased. Two module files sharing
// a key are ambiguous under an extension-less import on a case-insensitive FS. This
// subsumes plain "differ only by case" collisions (identical extension) too.
describe("filesystem case portability", () => {
  it("no two src modules share a case-insensitive, extension-less path", () => {
    const modules = Object.keys(import.meta.glob("./**/*.{ts,tsx,js,jsx,mts,cts}"));
    const byKey = new Map<string, string[]>();
    for (const file of modules) {
      const key = file.replace(/\.[^/.]+$/, "").toLowerCase();
      byKey.set(key, [...(byKey.get(key) ?? []), file]);
    }
    const collisions = [...byKey.values()].filter((group) => group.length > 1);
    expect(collisions).toEqual([]);
  });
});
