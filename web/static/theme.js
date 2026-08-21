// Applied before the first paint, which is why it is a blocking script in the
// head and not part of the application bundle: without it the page renders in
// the system theme and then jumps to the chosen one, and every load flashes
// the wrong colours.
//
// It is a file rather than an inline script so that the content security
// policy can be script-src 'self' with no exception for inline code.
(function () {
  try {
    var t = localStorage.getItem("weg:theme");
    if (t === "dark" || t === "light") document.documentElement.dataset.theme = t;
  } catch (e) {
    // Storage can be denied. The system theme is a fine answer.
  }
})();
