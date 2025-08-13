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
        wrapper._b_collect_name = None if callable(arg) and not isinstance(arg, str) else arg
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
                field = getattr(val, "_b_collect_name", None) or local_name
                field = _sanitize_field(field)
                items.append((val.__code__.co_firstlineno, field, val))

    items.sort(key=lambda t: t[0])
    used = {}
    fields, values = [], []
    for _, field, val in items:
        i = used.get(field, 0)
        final = field if i == 0 else f"{field}_{i}"
        used[field] = i + 1
        fields.append(final)
        values.append(val)

    NT = collections.namedtuple("NestedFunctions", fields or ["_empty"])
    return NT(*values) if values else NT(None)
