/* Tailwind is compiled once by assets/tailwind.sh and the result is committed
   as assets/tailwind.css. Nothing here runs in a browser or in the build of the
   Go binary; see docs/adr/0011.

   The globs are the whole answer to "which classes exist". Every file that can
   put a class on an element has to be listed, and today that is the templates
   and app.js, which builds a subtask row and toggles colour classes on the WIP
   badge. No Go file writes a class name, and a test in internal/web asserts
   that stays true. */
module.exports = {
  content: [
    './internal/web/templates/**/*.html',
    './assets/app.js',
  ],
  /* The theme is a class on <html>, set by the inline script in layout.html
     before the first paint. The default here is the prefers-color-scheme media
     query, which would ignore the toggle. */
  darkMode: 'class',
  theme: { extend: {} },
  plugins: [],
}
