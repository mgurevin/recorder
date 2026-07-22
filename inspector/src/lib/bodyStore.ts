const fileBodyReference = /^filebody:v1:([0-9a-f]{32})$/;

/**
 * Returns the path relative to a FileBodyStore root for a recognized opaque
 * reference. Absolute store roots intentionally never enter HAR evidence.
 */
export function fileBodyAssetPath(reference: string | undefined): string | undefined {
  if (!reference) return undefined;

  const match = fileBodyReference.exec(reference);
  return match ? `assets/${match[1]}.body` : undefined;
}
