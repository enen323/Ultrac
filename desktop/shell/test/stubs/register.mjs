// register.mjs — loaded with node --import; installs hooks.mjs for every
// import that follows, which is how the child process running src/main.ts
// gets the fake electron and the forced spawn.

import { register } from "node:module";

register(new URL("./hooks.mjs", import.meta.url));
