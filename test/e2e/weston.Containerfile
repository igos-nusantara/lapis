# Headless weston (floating desktop shell) for screenshots.
FROM docker.io/library/archlinux:latest
RUN pacman -Syu --noconfirm weston && pacman -Scc --noconfirm
