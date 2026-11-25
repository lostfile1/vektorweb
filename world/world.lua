-- VektorWeb Nexus Plaza — Interactive Sandbox Script
-- Runs inside client luavm sandbox (15ms/frame CPU cap, zero OS/FS/network access)

local welcomed = false
local tele_cooldown = 0.0
local timer = 0.0

local function dist3(ax, ay, az, bx, by, bz)
    local dx, dy, dz = ax - bx, ay - by, az - bz
    return math.sqrt(dx*dx + dy*dy + dz*dz)
end

function on_tick(dt)
    timer = timer + dt
    if tele_cooldown > 0 then
        tele_cooldown = tele_cooldown - dt
    end

    local x, y, z = world.player_pos()

    if not welcomed then
        welcomed = true
        world.notify("Welcome to VektorWeb Nexus Plaza! Climb the East stairs, test parkour West, or step on the Center Pad.")
    end

    local zone = "CENTRAL PLAZA"
    if dist3(x, y, z, 0.0, 2.2, 0.0) < 3.6 then
        zone = "NEXUS SPIRE (Jump Pad Active!)"
    elseif dist3(x, y, z, 0.0, 1.6, -16.0) < 3.2 then
        zone = "NORTH PORTAL GATE (Offline Sandbox Loop)"
    elseif x > 11.5 and y > 2.8 then
        zone = "EAST OBSERVATION MEZZANINE"
    elseif x < -10.5 and z > 7.5 then
        zone = "WEST PARKOUR COURSE"
    elseif x < -10.5 and math.abs(z) <= 6.5 then
        zone = "WEST AMPHITHEATER"
    end

    world.set_hud(string.format("%s  |  XYZ: %.1f, %.1f, %.1f", zone, x, y, z))

    if tele_cooldown <= 0 and dist3(x, y, z, 0.0, 2.15, 0.0) < 1.35 then
        tele_cooldown = 3.0
        world.notify("[BOOST PAD]: Warped to East Observation Mezzanine!")
        world.teleport(15.5, 5.0, 0.0)
    end
end
