from example.example import greeting
from fastbytes import xor_bytes


if __name__ == "__main__":
    print(greeting("polyglot"))
    print(xor_bytes(b"abc", 1).hex())
