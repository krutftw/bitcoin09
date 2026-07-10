export function redactDiscordPath(path) {
  const segments = String(path).split("/");
  if ((segments[1] === "interactions" || segments[1] === "webhooks") && segments.length > 3) {
    segments[3] = "[redacted]";
  }
  return segments.join("/");
}
