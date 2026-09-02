// world `consumer` imports the `greeter` interface. componentize-js turns a
// WIT import into a JS module import whose specifier is the fully qualified
// interface name, version included.
import { greet, greetAll } from 'wazy:compose/greeter@0.1.0';

// `run` is exported at the world's top level, so it is a bare named export.
export function run() {
  // Nothing below is hardcoded provider text: every string in the result
  // crossed the component boundary.
  const one = greet({ name: 'wazy', id: 42 });
  const two = greetAll(['a', 'b'])[0];
  const empty = greetAll([]);
  return [one, two, `empty-len=${empty.length}`];
}
