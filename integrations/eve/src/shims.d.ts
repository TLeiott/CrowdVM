// Minimal Node typings so the package builds without @types/node.
declare const Buffer: {
  from(data: string | ArrayLike<number>, enc?: string): { toString(enc?: string): string } & Uint8Array;
};
