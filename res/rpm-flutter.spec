Name:       p2pdesk
Version:    1.4.9
Release:    0
Summary:    RPM package
License:    AGPL-3.0-only
URL:        https://github.com/Hexrotor/p2pdesk
Vendor:     Hexrotor <Hexrotor@users.noreply.github.com>
Requires:   gtk3 libxcb libXfixes alsa-lib libva pam gstreamer1-plugins-base
Recommends: libayatana-appindicator-gtk3 libxdo
Provides:   libdesktop_drop_plugin.so()(64bit), libdesktop_multi_window_plugin.so()(64bit), libfile_selector_linux_plugin.so()(64bit), libflutter_custom_cursor_plugin.so()(64bit), libflutter_linux_gtk.so()(64bit), libscreen_retriever_plugin.so()(64bit), libtray_manager_plugin.so()(64bit), liburl_launcher_linux_plugin.so()(64bit), libwindow_manager_plugin.so()(64bit), libwindow_size_plugin.so()(64bit), libtexture_rgba_renderer_plugin.so()(64bit)

# https://docs.fedoraproject.org/en-US/packaging-guidelines/Scriptlets/

%description
The best open-source remote desktop client software, written in Rust.

%prep
# we have no source, so nothing here

%build
# we have no source, so nothing here

# %global __python %{__python3}

%install

mkdir -p "%{buildroot}/usr/share/p2pdesk" && cp -r ${HBB}/flutter/build/linux/x64/release/bundle/* -t "%{buildroot}/usr/share/p2pdesk"
mkdir -p "%{buildroot}/usr/bin"
install -Dm 644 $HBB/res/p2pdesk.service -t "%{buildroot}/usr/share/p2pdesk/files"
install -Dm 644 $HBB/res/p2pdesk.desktop -t "%{buildroot}/usr/share/p2pdesk/files"
install -Dm 644 $HBB/res/p2pdesk-link.desktop -t "%{buildroot}/usr/share/p2pdesk/files"
install -Dm 644 $HBB/res/128x128@2x.png "%{buildroot}/usr/share/icons/hicolor/256x256/apps/p2pdesk.png"
install -Dm 644 $HBB/res/scalable.svg "%{buildroot}/usr/share/icons/hicolor/scalable/apps/p2pdesk.svg"

%files
/usr/share/p2pdesk/*
/usr/share/p2pdesk/files/p2pdesk.service
/usr/share/icons/hicolor/256x256/apps/p2pdesk.png
/usr/share/icons/hicolor/scalable/apps/p2pdesk.svg
/usr/share/p2pdesk/files/p2pdesk.desktop
/usr/share/p2pdesk/files/p2pdesk-link.desktop

%changelog
# let's skip this for now

%pre
# can do something for centos7
case "$1" in
  1)
    # for install
  ;;
  2)
    # for upgrade
    systemctl stop p2pdesk || true
  ;;
esac

%post
cp /usr/share/p2pdesk/files/p2pdesk.service /etc/systemd/system/p2pdesk.service
cp /usr/share/p2pdesk/files/p2pdesk.desktop /usr/share/applications/
cp /usr/share/p2pdesk/files/p2pdesk-link.desktop /usr/share/applications/
ln -sf /usr/share/p2pdesk/p2pdesk /usr/bin/p2pdesk
systemctl daemon-reload
systemctl enable p2pdesk
systemctl start p2pdesk
update-desktop-database

%preun
case "$1" in
  0)
    # for uninstall
    systemctl stop p2pdesk || true
    systemctl disable p2pdesk || true
    rm /etc/systemd/system/p2pdesk.service || true
  ;;
  1)
    # for upgrade
  ;;
esac

%postun
case "$1" in
  0)
    # for uninstall
    rm /usr/bin/p2pdesk || true
    rmdir /usr/lib/p2pdesk || true
    rmdir /usr/local/p2pdesk || true
    rmdir /usr/share/p2pdesk || true
    rm /usr/share/applications/p2pdesk.desktop || true
    rm /usr/share/applications/p2pdesk-link.desktop || true
    update-desktop-database
  ;;
  1)
    # for upgrade
    rmdir /usr/lib/p2pdesk || true
    rmdir /usr/local/p2pdesk || true
  ;;
esac
