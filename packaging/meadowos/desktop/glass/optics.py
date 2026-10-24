"""Original CPU material prototype inspired by Liquid Glass Studio principles.

Separate backdrop/blurred textures, SDF edge normals and displaced samples.
No Studio source code or demo artwork is copied. Foreground is never an input.
"""
from dataclasses import dataclass

import numpy as np
from PIL import Image, ImageFilter


@dataclass(frozen=True)
class Material:
    blur: float = 14.0
    tint: float = .22
    radius: float = 16.0
    radii: tuple | None = None  # TL, TR, BR, BL; None uses scalar radius.
    bevel: float = 12.0
    refraction: float = 9.0
    dispersion: float = .35
    highlight: float = .14
    tint_color: tuple = (249, 249, 249)  # BGR, matching the capture buffer.


def _sample(texture, x, y):
    x = np.clip(x, 0, texture.shape[1] - 1.001)
    y = np.clip(y, 0, texture.shape[0] - 1.001)
    x0, y0 = x.astype(np.int32), y.astype(np.int32)
    x1, y1 = np.minimum(x0 + 1, texture.shape[1] - 1), np.minimum(y0 + 1, texture.shape[0] - 1)
    wx, wy = (x - x0)[..., None], (y - y0)[..., None]
    return ((texture[y0, x0] * (1 - wx) + texture[y0, x1] * wx) * (1 - wy)
            + (texture[y1, x0] * (1 - wx) + texture[y1, x1] * wx) * wy)


def render(backdrop, width, height, margin=40, material=Material()):
    """Return premultiplied BGRA for one rounded native region plus optical metrics."""
    if backdrop.shape != (height + margin * 2, width + margin * 2, 4):
        raise ValueError("Backdrop must include the requested optical margin")
    rgb = backdrop[..., :3]
    blurred = np.asarray(Image.fromarray(rgb).filter(ImageFilter.GaussianBlur(material.blur)), dtype=np.float32)
    # Joined panes have no internal rim. Avoid allocating full SDF/refraction
    # maps for a flat content surface; the native owner clips its outer edge.
    flat_corners = not material.radius and (material.radii is None or not any(material.radii))
    if flat_corners and not any((material.bevel, material.refraction, material.dispersion, material.highlight)):
        output = np.empty((height, width, 4), dtype=np.uint8)
        center = blurred[margin:margin+height, margin:margin+width]
        output[..., :3] = np.clip(center * (1-material.tint)
                                  + np.asarray(material.tint_color) * material.tint, 0, 255).astype(np.uint8)
        output[..., 3] = 255
        return output
    yy, xx = np.mgrid[:height, :width].astype(np.float32)
    if material.radii is None:
        radius = min(material.radius, width / 2, height / 2)
    else:
        tl, tr, br, bl = material.radii
        radius = np.where(yy < height / 2, np.where(xx < width / 2, tl, tr),
                          np.where(xx < width / 2, bl, br))
        radius = np.clip(radius, 0, min(width, height) / 2)
    qx = abs(xx + .5 - width / 2) - (width / 2 - radius)
    qy = abs(yy + .5 - height / 2) - (height / 2 - radius)
    distance = np.hypot(np.maximum(qx, 0), np.maximum(qy, 0)) + np.minimum(np.maximum(qx, qy), 0) - radius
    # Finite differences of the rounded-rectangle signed distance yield normals.
    ny, nx = np.gradient(distance)
    length = np.maximum(np.hypot(nx, ny), .001)
    nx, ny = nx / length, ny / length
    depth = np.clip(-distance / max(material.bevel, 1), 0, 1)
    edge = (1 - depth) ** 2
    displacement = np.sin(depth * np.pi) * material.refraction * (1 - depth)
    sx, sy = xx + margin - nx * displacement, yy + margin - ny * displacement
    center = _sample(blurred, sx, sy)
    clear = _sample(rgb.astype(np.float32), sx, sy)
    # A narrow clearer bevel makes the moving background bend at the glass edge.
    result = center * (1 - edge[..., None] * .82) + clear * edge[..., None] * .82
    if material.dispersion:
        for channel, sign in ((0, -1), (2, 1)):
            chroma = _sample(rgb.astype(np.float32), sx + nx * edge * material.dispersion * sign,
                             sy + ny * edge * material.dispersion * sign)
            result[..., channel] += (chroma[..., channel] - clear[..., channel]) * edge
    result = result * (1 - material.tint) + np.asarray(material.tint_color) * material.tint
    lighting = np.clip((-nx * .6 - ny * .8) * .5 + .5, 0, 1)
    highlight = edge * lighting * material.highlight
    result += (255 - result) * highlight[..., None]
    alpha = np.clip(.5 - distance, 0, 1)
    out = np.empty((height, width, 4), dtype=np.uint8)
    out[..., :3] = (np.clip(result, 0, 255) * alpha[..., None]).astype(np.uint8)
    out[..., 3] = (alpha * 255).astype(np.uint8)
    return out


def render_joined(backdrop, width, height, split, margin=40, dark=False):
    """One continuous Files backdrop, with a denser content material.

    Coordinates and blur sampling are shared across the join. There is no
    internal bevel, rounded mask or transparent gap; the native window owns
    the outer clip. Pixels are already composited against actual lower windows
    and remain opaque so GTK foreground controls are never blurred.
    """
    if backdrop.shape != (height + margin * 2, width + margin * 2, 4):
        raise ValueError("Backdrop must include the requested optical margin")
    if not 0 <= split <= width:
        raise ValueError("Split must lie inside the surface")
    source = Image.fromarray(backdrop[..., :3])
    output = np.empty((height, width, 4), dtype=np.uint8)
    output[..., 3] = 255
    # The capture buffer is BGRA, so this dark target is #202830 in RGB.
    tint_color = np.array((48, 40, 32) if dark else (249, 249, 249), dtype=np.float32)
    for left, right, blur, tint in ((0, split, 34, .80 if dark else .76), (split, width, 28, .68)):
        if left == right:
            continue
        # Blur only the panel plus its optical margin. This bounds working
        # memory and avoids two full-frame refraction/normal maps.
        region = source.crop((left, 0, right + 2 * margin, height + 2 * margin))
        region = region.filter(ImageFilter.GaussianBlur(blur))
        pixels = np.asarray(region.crop((margin, margin, right-left+margin, height+margin)),
                            dtype=np.float32)
        output[:, left:right, :3] = (pixels * (1-tint) + tint_color*tint).astype(np.uint8)
    return output
