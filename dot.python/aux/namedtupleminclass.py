import inspect
import collections
from functools import wraps
import keyword
import re

def collectable(arg=None):
    """
    Use as either:
      @collectable
      def f(): ...

      @collectable("alias")
      def f(): ...
    """
    def deco(fn):
        @wraps(fn)
        def wrapper(*args, **kwargs):
            return fn(*args, **kwargs)
        # Mark for b()
        wrapper._b_collect = True
        wrapper._b_collect_name = arg if isinstance(arg, str) else fn.__name__
        wrapper._b_fn = fn
        return wrapper

    # If used without (), arg is actually the function object
    if callable(arg) and not isinstance(arg, str):
        return deco(arg)
    return deco

def _sanitize_field(n: str) -> str:
    n = re.sub(r'\W|^(?=\d)', '_', n)  # valid identifier
    if not n or keyword.iskeyword(n):
        n = f'_{n}'
    return n

def b():
    """
    Return a namedtuple('NestedFunctions', ...) of the caller's *decorated* nested functions.
    - Only @collectable-marked functions are included.
    - Field order follows source definition order.
    - Field names: alias if provided, else local name; sanitized to valid identifiers.
    - De-duplicates identical field names by suffixing _1, _2, ...
    """
    frame = inspect.currentframe().f_back
    caller_name = frame.f_code.co_name
    prefix = f"{caller_name}.<locals>."

    items = []
    for local_name, val in frame.f_locals.items():
        if inspect.isfunction(val) and getattr(val, "_b_collect", False):
            qn = getattr(val, "__qualname__", "")
            if ".<locals>." in qn and qn.startswith(prefix):
                name = val._b_collect_name
                field = local_name if name == '<lambda>' else name
                fn = val._b_fn
                items.append((val.__code__.co_firstlineno, field, fn))

    items.sort(key=lambda t: t[0])
    used = {}
    fields, values = [], []
    for _, field, val in items:
        i = used.get(field, 0)
        final = field if i == 0 else f"{field}_{i}"
        used[field] = i + 1
        fields.append(final)
        print(f'{i}:{final}')
        values.append(val)

    NT = collections.namedtuple("NestedFunctions", fields or ["_empty"])
    return NT(*values) if values else NT(None)


def outer():
    @collectable  # no ()
    def f(): return "f()"

    @collectable("twice")
    def g(x): return x * 2

    @collectable()
    def h(s): return s.upper()

    four = collectable('rabbit')(lambda: 'rabbit')

    return b()

a=outer()
print(dir(a.f))
print(dir(a.twice))
print(dir(a.h))
print(f'f:{a.f()}')
print(f'twice:{a.twice(4)}')
print(f'h:{a.h("dog")}')

print(dir(outer))
print(outer.__name__)
print(outer)

print(a.rabbit)
print(a.rabbit())
