import { open, realpath } from "node:fs/promises";
import { constants } from "node:fs";
import { basename, isAbsolute } from "node:path";
import type { InputPart } from "./protocol.ts";

export interface AttachmentFile {
  stat(): Promise<{ size: number; isFile(): boolean }>;
  read(
    buffer: Uint8Array,
    offset: number,
    length: number,
    position: number,
  ): Promise<{ bytesRead: number }>;
  close(): Promise<void>;
}
export interface AttachmentFiles {
  realpath(path: string): Promise<string>;
  open(path: string): Promise<AttachmentFile>;
}
export interface AttachmentOptions {
  files?: AttachmentFiles;
  signal?: AbortSignal;
  maxFiles?: number;
  maxTextBytes?: number;
  maxImageBytes?: number;
  maxTotalTextBytes?: number;
  maxTotalImageBytes?: number;
}
export interface Attachment {
  path: string;
  name: string;
  kind: "text" | "image";
  size: number;
}
const localFiles: AttachmentFiles = {
  realpath,
  open: (path) => open(path, constants.O_RDONLY | constants.O_NONBLOCK),
};
function isImage(bytes: Buffer): boolean {
  return (
    bytes.subarray(0, 8).equals(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10])) ||
    (bytes[0] === 255 && bytes[1] === 216 && bytes[2] === 255) ||
    ["GIF87a", "GIF89a"].includes(bytes.subarray(0, 6).toString()) ||
    (bytes.subarray(0, 4).toString() === "RIFF" && bytes.subarray(8, 12).toString() === "WEBP")
  );
}
async function readFiles(paths: string[], options: AttachmentOptions, contents: boolean) {
  const limits = {
    maxFiles: options.maxFiles ?? 20,
    maxTextBytes: options.maxTextBytes ?? 512 * 1024,
    maxImageBytes: options.maxImageBytes ?? 5 * 1024 * 1024,
    maxTotalTextBytes: options.maxTotalTextBytes ?? 1024 * 1024,
    maxTotalImageBytes: options.maxTotalImageBytes ?? 20 * 1024 * 1024,
  };
  for (const value of Object.values(limits))
    if (!Number.isSafeInteger(value) || value < 0)
      throw new RangeError("Attachment limits must be nonnegative safe integers.");
  if (!Array.isArray(paths) || paths.length > limits.maxFiles)
    throw new Error(`Choose up to ${limits.maxFiles} files.`);
  const files = options.files ?? localFiles;
  const result: (Attachment & { contents?: string })[] = [];
  const seen = new Set<string>();
  let textBytes = 0,
    imageBytes = 0;
  for (const selected of paths) {
    options.signal?.throwIfAborted();
    if (typeof selected !== "string" || !isAbsolute(selected))
      throw new Error("Choose an absolute file path.");
    const path = await files.realpath(selected);
    if (seen.has(path)) continue;
    seen.add(path);
    const name = basename(path),
      file = await files.open(path);
    try {
      const stat = await file.stat();
      if (!stat.isFile()) throw new Error(`${name} is not a regular file.`);
      const header = Buffer.alloc(12);
      await file.read(header, 0, 12, 0);
      const kind = isImage(header) ? "image" : "text";
      const limit = kind === "image" ? limits.maxImageBytes : limits.maxTextBytes;
      if (stat.size > limit)
        throw new Error(`${name} is too large (${stat.size} bytes; limit ${limit}).`);
      const bytes = Buffer.alloc(Math.min(stat.size, limit) + 1);
      let length = 0;
      while (length < bytes.length) {
        options.signal?.throwIfAborted();
        const { bytesRead } = await file.read(bytes, length, bytes.length - length, length);
        if (!bytesRead) break;
        length += bytesRead;
      }
      if (length > Math.min(stat.size, limit))
        throw new Error(`${name} changed while reading; select it again.`);
      let text: string | undefined;
      if (kind === "text") {
        try {
          const data = bytes.subarray(0, length);
          if (data.includes(0)) throw new Error("binary");
          text = new TextDecoder("utf-8", { fatal: true }).decode(data);
        } catch {
          throw new Error(`${name}: choose UTF-8 text/code or a PNG, JPEG, GIF or WebP image.`);
        }
        textBytes += length;
      } else imageBytes += length;
      if (textBytes > limits.maxTotalTextBytes || imageBytes > limits.maxTotalImageBytes)
        throw new Error("Attachments exceed the configured total byte limit.");
      result.push({
        path,
        name,
        kind,
        size: length,
        ...(contents && kind === "text" ? { contents: text } : {}),
      });
    } finally {
      await file.close();
    }
  }
  return result;
}
/** Inspect local selections without retaining their contents. Does not send a prompt. */
export async function inspectAttachments(
  paths: string[],
  options: AttachmentOptions = {},
): Promise<Attachment[]> {
  return readFiles(paths, options, false);
}
/** Read attachments at send time. Text is inlined; images are local paths, so
 * the runtime must share this filesystem. Policies and limits belong to the host. */
export async function prepareAttachments(
  prompt: string | InputPart[],
  paths: string[],
  options: AttachmentOptions = {},
): Promise<{ input: InputPart[]; attachments: Attachment[] }> {
  const files = await readFiles(paths, options, true);
  return {
    input: [
      ...(typeof prompt === "string" ? [{ type: "text" as const, text: prompt }] : prompt),
      ...files.map((file) =>
        file.kind === "image"
          ? { type: "image" as const, path: file.path }
          : {
              type: "text" as const,
              text: `Attached file ${JSON.stringify(file.path)} (context):\n${file.contents}`,
            },
      ),
    ],
    attachments: files.map(({ contents: _contents, ...metadata }) => metadata),
  };
}
