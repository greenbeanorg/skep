// Sets data-theme on <html> before the first paint, from the stored choice
// ("light", "dark", or "system"; absent means system). Loaded synchronously
// from index.html. Reading storage can throw in a private window or with
// blocked storage; then the system preference is used. Plain script, no
// module: it must run before anything renders. The React ThemeProvider
// (src/app/theme.tsx) takes over from here and keeps the attribute in step.
(function () {
  var choice = "system";
  try {
    var stored = window.localStorage.getItem("skep-theme");
    if (stored === "light" || stored === "dark") choice = stored;
  } catch (e) {
    // Fall back to the system preference.
  }
  var dark =
    choice === "dark" ||
    (choice === "system" &&
      typeof window.matchMedia === "function" &&
      window.matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
})();
