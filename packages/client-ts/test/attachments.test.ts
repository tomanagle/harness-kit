import test from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { prepareAttachments, inspectAttachments } from "../src/node.ts";
test("attachments deduplicate, preserve prompt formatting, and enforce limits", async () => {
  const dir = await mkdtemp(join(tmpdir(), "kit-attachments-"));
  try {
    const file = join(dir, "code.ts");
    await writeFile(file, "const x = 1;\n");
    const inspected = await inspectAttachments([file, file]);
    assert.equal(inspected.length, 1);
    assert.equal("contents" in inspected[0]!, false);
    const prepared = await prepareAttachments("  Explain\n", [file, file]);
    assert.equal(prepared.input.length, 2);
    assert.equal(prepared.input[0]?.text, "  Explain\n");
    assert.match(prepared.input[1]!.text!, /const x = 1;/);
    await assert.rejects(prepareAttachments("x", [file], { maxTextBytes: 1 }), /too large/);
    await assert.rejects(prepareAttachments("x", [file], { maxTotalTextBytes: 1 }), /total/);
    await writeFile(file, Buffer.from([0xff, 0]));
    await assert.rejects(inspectAttachments([file]), /UTF-8/);
    await assert.rejects(inspectAttachments([dir]), /regular file/);
    await assert.rejects(inspectAttachments([file], { signal: AbortSignal.abort() }));
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});
