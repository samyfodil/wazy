// world `provider` exports the whole `greeter` interface, so the module
// exports one object named after the interface. jco maps WIT kebab-case to
// lowerCamelCase: `greet-all` -> `greetAll`, and the `visitor` record arrives
// as a plain object with `name` and `id` properties.
export const greeter = {
  greet(who) {
    return `Hello, ${who.name} #${who.id}! (from JavaScript)`;
  },

  greetAll(names) {
    // map() over an empty array yields an empty array -- the empty-list path
    // is deliberately not special-cased.
    return names.map((name) => `${name} (via JavaScript)`);
  },
};
