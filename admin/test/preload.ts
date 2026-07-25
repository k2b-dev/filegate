// bun test runs the TSX sources directly, without the Bun.build step that
// normally applies the SSR plugin, so Solid's JSX transform has to be registered
// here or every rendered page throws "React is not defined".
import { plugin } from "../src/config";

Bun.plugin(plugin());
