// The `greeter` world exports one function, so this module exports one
// function. jco maps the WIT kebab-case name to lowerCamelCase; `greet` is
// already both.
export function greet(name) {
  return `Hello, ${name}! (from JavaScript)`;
}
