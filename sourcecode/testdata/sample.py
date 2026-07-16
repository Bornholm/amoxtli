"""Sample module."""


def greet(name):
    """Say hello to the given name."""
    return f"hello {name}"


class Server:
    """A tiny server."""

    def __init__(self, addr):
        self._addr = addr

    @property
    def addr(self):
        """Return the bind address."""
        return self._addr


def farewell(name):
    return f"goodbye {name}"
