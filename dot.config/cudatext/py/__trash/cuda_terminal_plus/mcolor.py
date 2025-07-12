import colorsys

from cudax_lib import int_to_html_color


class MColor:
    def __init__(self, hexcol=None, src=None):
        if hexcol is not None:
            self.set_hexcol(hexcol)
        elif src is not None:
            self.set_hexcol(src.hexcol())
        
    def set_hsv(self, hsv):
        self._h, self._s, self._v = hsv
        self._hexcol = MColor.rgb_to_hex(*colorsys.hsv_to_rgb(self._h, self._s, self._v))
        
    def set_hexcol(self, hexcol):
        self._hexcol = hexcol
        self._h, self._s, self._v = colorsys.rgb_to_hsv(*MColor.hex_to_rgb(hexcol))
        
    def hexcol(self):
        return self._hexcol
        
    def hsv(self):
        return self._h, self._s, self._v
    
    def h(self, add=None):
        if add is not None:
            h = max(0, min(1, self._h + add))
            self.set_hsv((h, self._s, self._v))
        else:
            return self._h
    
    def s(self, add=None):
        if add is not None:
            s = max(0, min(1, self._s + add))
            self.set_hsv((self._h, s, self._v))
        else:
            return self._s
    
    def v(self, add=None):
        if add is not None:
            v = max(0, min(1, self._v + add))
            self.set_hsv((self._h, self._s, v))
        else:
            return self._v
            
    def __str__(self):
        return 'mcol:'+int_to_html_color(self._hexcol)
            
    def hex_to_rgb(col):  
        return (col&0xff)/0xff, ((col&0xff00)>>8)/0xff, ((col&0xff0000)>>16)/0xff
    
    def rgb_to_hex(r,g,b):
        return (round(b*255) << 16) + (round(g*0xff) << 8) + round(r*0xff)