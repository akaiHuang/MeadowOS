import unittest

import numpy as np

from glass.optics import Material, render


class MaterialBoundaryTests(unittest.TestCase):
    def test_directional_sidebar_corners_leave_inner_edge_square(self):
        backdrop = np.full((170, 160, 4), 255, dtype=np.uint8)
        result = render(backdrop, 80, 90, material=Material(radii=(16, 0, 0, 16)))
        self.assertEqual(int(result[0, 0, 3]), 0)
        self.assertEqual(int(result[-1, 0, 3]), 0)
        self.assertEqual(int(result[0, -1, 3]), 255)
        self.assertEqual(int(result[-1, -1, 3]), 255)

    def test_dispersion_preserves_premultiplied_alpha_on_sharp_background(self):
        random = np.random.default_rng(2)
        backdrop = np.full((120, 120, 4), 255, dtype=np.uint8)
        backdrop[random.random((120, 120)) < .01, :3] = 0
        result = render(backdrop, 40, 40)
        self.assertTrue(np.all(result[..., :3] <= result[..., 3, None]))

    def test_frost_reduces_real_backdrop_high_frequency(self):
        backdrop = np.full((140, 140, 4), 255, dtype=np.uint8)
        backdrop[:, ::2, :3] = 0
        result = render(backdrop, 60, 60, material=Material(blur=18, tint=.46, radius=0))
        source_variation = float(backdrop[60:80, 60:80, 0].std())
        self.assertLess(float(result[20:40, 20:40, 0].std()), source_variation * .1)

    def test_joined_flat_surface_has_no_internal_clear_bevel(self):
        backdrop = np.zeros((140, 140, 4), dtype=np.uint8)
        backdrop[..., 3] = 255
        backdrop[:, ::2, :3] = 255
        material = Material(radius=0, blur=28, tint=.68, bevel=0, refraction=0,
                            dispersion=0, highlight=0, tint_color=(48, 40, 32))
        result = render(backdrop, 60, 60, material=material)
        self.assertTrue(np.all(result[..., 3] == 255))
        # Finite capture margins retain a small Gaussian boundary variation;
        # a clear one-pixel rim would instead restore the sharp black/white.
        self.assertLessEqual(int(np.ptp(result[..., 0])), 8)
        self.assertLessEqual(abs(int(result[30, 0, 0])-int(result[30, 30, 0])), 8)


if __name__ == "__main__":
    unittest.main()
