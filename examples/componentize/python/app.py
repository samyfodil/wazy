"""The `wazy:examples/greeter` world, in Python.

componentize-py generates the `wit_world` module from greet.wit; implementing
its `WitWorld` protocol is the whole guest.
"""

import wit_world


class WitWorld(wit_world.WitWorld):
    def greet(self, name: str) -> str:
        return f"Hello, {name}! (from Python)"
