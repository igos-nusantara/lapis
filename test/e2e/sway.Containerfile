# Headless sway for e2e runs on hosts without a Wayland desktop. sway's
# file capability (cap_sys_nice) would make exec fail for a non-root user.
FROM docker.io/library/archlinux:latest
RUN pacman -Syu --noconfirm sway grim libcap && setcap -r /usr/bin/sway && pacman -Scc --noconfirm
