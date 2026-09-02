"""The `provider` world of `wazy:compose@0.1.0`, in Python.

componentize-py turns the WIT `greeter` interface into `wit_world.exports.Greeter`
(the protocol to implement) plus `wit_world.exports.greeter` (the type module
holding the `visitor` record). The class name must match the interface name.
"""

from typing import List

import wit_world
from wit_world.exports import greeter

LANG = "Python"


class Greeter(wit_world.exports.Greeter):
    def greet(self, who: greeter.Visitor) -> str:
        return f"Hello, {who.name} #{who.id}! (from {LANG})"

    def greet_all(self, names: List[str]) -> List[str]:
        # No special case for the empty list: the comprehension already
        # returns [], which is exactly what the contract asks for.
        return [f"{name} (via {LANG})" for name in names]
