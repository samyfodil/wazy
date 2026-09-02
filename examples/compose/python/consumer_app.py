"""The `consumer` world of `wazy:compose@0.1.0`, in Python.

Everything this returns comes back across the imported `greeter` interface —
no provider text is hardcoded here, so a bad lift or lower shows up in the
output rather than being masked by a local string.
"""

from typing import List

import wit_world
from wit_world.imports import greeter
from wit_world.imports.greeter import Visitor


class WitWorld(wit_world.WitWorld):
    def run(self) -> List[str]:
        # 1. a record with a string field, crossing the composition boundary
        first = greeter.greet(Visitor(name="wazy", id=42))
        # 2. list<string> in both directions
        second = greeter.greet_all(["a", "b"])[0]
        # 3. the empty-list path; report the real length, whatever it is
        empty = greeter.greet_all([])
        third = f"empty-len={len(empty)}"
        return [first, second, third]
