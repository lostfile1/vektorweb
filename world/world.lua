-- Sandboxed world.lua (zero os/io/network access, 15ms/frame CPU cap)
local greeted = false

function on_tick(dt)
    local x, y, z = world.player_pos()
    world.set_hud(string.format("VEKTORWEB PLAZA // POS: %.1f, %.1f, %.1f", x, y, z))

    if not greeted then
        greeted = true
        world.notify("Welcome! Edit world.toml, environment.glb, or world.lua in your world folder.")
    end
end
