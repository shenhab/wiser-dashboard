// Enclosure for the hot water tank sensors: Shelly 1 Gen3 + Plus Add-on,
// three Wago 221-615 connectors and a DC barrel-jack adapter.
//
// Print in PETG or ASA, not PLA: airing cupboards get warm enough to soften PLA.
// 0.2 mm layers, 3 walls, 15% infill, no supports. Base prints open side up,
// lid prints flat side down. Closes with four M3 x 10 self-tapping screws.
//
// Measure your parts before printing and adjust `inner` and the slot sizes.

part = "both"; // "base", "lid" or "both"

inner = [95, 65, 40];   // inside length, width, height (mm)
wall = 2.4;
floor_t = 2;
lid_t = 2;
lip = 3;                // how far the lid's lip drops inside the walls
fit = 0.3;              // clearance between the lid lip and the walls
corner_r = 3;

boss_d = 7;
screw_pilot = 2.6;      // M3 self-tapping into the bosses
screw_clear = 3.4;
screw_depth = 12;

probe_cables = 5;
probe_slot = 4.6;       // probe cable diameter + ~0.6
power_slot = 6;         // DC lead diameter + ~0.6
slot_gap = 4;

ear_r = 6;
ear_t = 3;
ear_hole = 4.5;

outer = [inner.x + 2*wall, inner.y + 2*wall, inner.z + floor_t];
boss_offset = wall + boss_d/2 - 1; // overlaps the walls by 1 mm
boss_pos = [for (x = [boss_offset, outer.x - boss_offset],
                 y = [boss_offset, outer.y - boss_offset]) [x, y]];
ear_x = [outer.x * 0.25, outer.x * 0.75];

$fn = 48;

module rounded_box(size, r) {
    hull() for (x = [r, size.x - r], y = [r, size.y - r])
        translate([x, y, 0]) cylinder(r = r, h = size.z);
}

// A U-shaped cable slot through the x = 0 wall. The cable sits below the lid's
// lip, so closing the lid holds it without pinching.
module cable_slot(y, w) {
    depth = lip + w + 0.5;
    translate([-1, y, outer.z - depth + w/2]) rotate([0, 90, 0])
        cylinder(d = w, h = wall + 2);
    translate([-1, y - w/2, outer.z - depth + w/2]) cube([wall + 2, w, depth]);
}

module cable_slots() {
    widths = concat([for (i = [1:probe_cables]) probe_slot], [power_slot]);
    total = probe_cables * probe_slot + power_slot + probe_cables * slot_gap;
    y0 = outer.y/2 - total/2;
    for (i = [0:len(widths) - 1]) {
        before = i * slot_gap + (i == 0 ? 0 : i * probe_slot);
        cable_slot(y0 + before + widths[i]/2, widths[i]);
    }
}

module ears() {
    for (x = ear_x, side = [0, 1]) {
        y = side == 0 ? -ear_r : outer.y + ear_r;
        hull() {
            translate([x, y, 0]) cylinder(r = ear_r, h = ear_t);
            translate([x - ear_r, side == 0 ? 0 : outer.y - 1, 0]) cube([2*ear_r, 1, ear_t]);
        }
    }
}

module base() {
    difference() {
        union() {
            rounded_box(outer, corner_r);
            ears();
        }
        translate([wall, wall, floor_t]) cube([inner.x, inner.y, inner.z + 1]);
        cable_slots();
        for (x = ear_x, y = [-ear_r, outer.y + ear_r])
            translate([x, y, -1]) cylinder(d = ear_hole, h = ear_t + 2);
    }
    for (p = boss_pos) translate([p.x, p.y, 0]) difference() {
        cylinder(d = boss_d, h = outer.z);
        translate([0, 0, outer.z - screw_depth]) cylinder(d = screw_pilot, h = screw_depth + 1);
    }
}

module lid() {
    difference() {
        union() {
            rounded_box([outer.x, outer.y, lid_t], corner_r);
            translate([0, 0, lid_t]) difference() {
                translate([wall + fit, wall + fit, 0])
                    cube([inner.x - 2*fit, inner.y - 2*fit, lip]);
                translate([wall + fit + 1.6, wall + fit + 1.6, -1])
                    cube([inner.x - 2*fit - 3.2, inner.y - 2*fit - 3.2, lip + 2]);
                for (p = boss_pos) translate([p.x, p.y, -1]) cylinder(d = boss_d + 1, h = lip + 2);
            }
        }
        for (p = boss_pos) translate([p.x, p.y, -1]) cylinder(d = screw_clear, h = lid_t + 2);
        // Vent slots
        for (i = [0:5]) translate([outer.x/2 - 26.5 + i * 10, outer.y/2 - 15, -1])
            cube([3, 30, lid_t + 2]);
    }
}

if (part == "base" || part == "both") base();
if (part == "lid" || part == "both")
    translate([0, part == "both" ? outer.y + 2*ear_r + 10 : 0, 0]) lid();
