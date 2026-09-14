export function hasUnpairedSurrogate(value) {
  if (typeof value !== "string") return false;
  for (let index = 0; index < value.length; index += 1) {
    const unit = value.charCodeAt(index);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const low = value.charCodeAt(index + 1);
      if (!(low >= 0xdc00 && low <= 0xdfff)) return true;
      index += 1;
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      return true;
    }
  }
  return false;
}

export function hasUnpairedSurrogateValue(value, seen = new Set()) {
  if (typeof value === "string") return hasUnpairedSurrogate(value);
  if (value === null || typeof value !== "object") return false;
  if (seen.has(value)) return false;
  seen.add(value);
  if (Array.isArray(value)) return value.some((entry) => hasUnpairedSurrogateValue(entry, seen));
  return Object.entries(value).some(([key, entry]) => hasUnpairedSurrogate(key) || hasUnpairedSurrogateValue(entry, seen));
}
