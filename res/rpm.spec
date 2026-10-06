Name:       p2pdesk
Version:    1.4.9
Release:    0
Summary:    RPM package
License:    AGPL-3.0-only
URL:        https://github.com/Hexrotor/p2pdesk
Vendor:     Hexrotor <Hexrotor@users.noreply.github.com>
Requires:   gtk3 libxcb libXfixes alsa-lib libva2 pam gstreamer1-plugins-base
Recommends: libayatana-appindicator-gtk3 libxdo

# https://docs.fedoraproject.org/en-US/packaging-guidelines/Scriptlets/

%description
The best open-source remote desktop client software, written in Rust.

%prep
# we have no source, so nothing here

%build
# we have no source, so nothing here

%global __python %{__python3}

%install
mkdir -p %{buildroot}/usr/bin/
mkdir -p %{buildroot}/usr/share/p2pdesk/
mkdir -p %{buildroot}/usr/share/p2pdesk/files/
mkdir -p %{buildroot}/usr/share/icons/hicolor/256x256/apps/
mkdir -p %{buildroot}/usr/share/icons/hicolor/scalable/apps/
install -m 755 $HBB/target/release/p2pdesk %{buildroot}/usr/bin/p2pdesk
install $HBB/libsciter-gtk.so %{buildroot}/usr/share/p2pdesk/libsciter-gtk.so
install $HBB/res/p2pdesk.service %{buildroot}/usr/share/p2pdesk/files/
install $HBB/res/128x128@2x.png %{buildroot}/usr/share/icons/hicolor/256x256/apps/p2pdesk.png
install $HBB/res/scalable.svg %{buildroot}/usr/share/icons/hicolor/scalable/apps/p2pdesk.svg
install $HBB/res/p2pdesk.desktop %{buildroot}/usr/share/p2pdesk/files/
install $HBB/res/p2pdesk-link.desktop %{buildroot}/usr/share/p2pdesk/files/

%files
/usr/bin/p2pdesk
/usr/share/p2pdesk/libsciter-gtk.so
/usr/share/p2pdesk/files/p2pdesk.service
/usr/share/icons/hicolor/256x256/apps/p2pdesk.png
/usr/share/icons/hicolor/scalable/apps/p2pdesk.svg
/usr/share/p2pdesk/files/p2pdesk.desktop
/usr/share/p2pdesk/files/p2pdesk-link.desktop
/usr/share/p2pdesk/files/__pycache__/*

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
    rm /usr/share/applications/p2pdesk.desktop || true
    rm /usr/share/applications/p2pdesk-link.desktop || true
    update-desktop-database
  ;;
  1)
    # for upgrade
  ;;
esac
