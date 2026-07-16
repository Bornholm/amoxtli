<?php

/** Say hello to the given name. */
function greet(string $name): string
{
    return "hello $name";
}

/** A tiny server. */
class Server
{
    /** Start the server. */
    public function start(): bool
    {
        return true;
    }
}

interface Handler
{
}

trait Loggable
{
}
