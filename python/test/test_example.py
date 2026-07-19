import asyncio

from example.example import greeting


def test_greeting() -> None:
  assert greeting("world") == "hello, world"


async def test_async_functions_are_supported() -> None:
  await asyncio.sleep(0)
