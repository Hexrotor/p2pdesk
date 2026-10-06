if(DEFINED ENV{USE_AOM_391})
    vcpkg_from_git(
        OUT_SOURCE_PATH SOURCE_PATH
        URL "https://aomedia.googlesource.com/aom"
        REF 8ad484f8a18ed1853c094e7d3a4e023b2a92df28 # 3.9.1 compatibility build
        PATCHES
            aom-uninitialized-pointer.diff
            aom-avx2.diff
            aom-install.diff
    )
else()
    vcpkg_from_git(
        OUT_SOURCE_PATH SOURCE_PATH
        URL "https://aomedia.googlesource.com/aom"
        REF de4c1d1edc49723a78954d30a83690aa1937422f # 3.15.0
    )
endif()

vcpkg_find_acquire_program(PERL)
if(VCPKG_TARGET_ARCHITECTURE MATCHES "^(x86|x64)$")
    vcpkg_find_acquire_program(NASM)
    set(aom_nasm_compiler "-DCMAKE_ASM_NASM_COMPILER=${NASM}")
endif()

set(aom_target_cpu "")
if(VCPKG_TARGET_IS_UWP OR (VCPKG_TARGET_IS_WINDOWS AND VCPKG_TARGET_ARCHITECTURE MATCHES "^arm"))
    # UWP + aom's assembler files result in weirdness and build failures
    # Also, disable assembly on ARM and ARM64 Windows to fix compilation issues.
    set(aom_target_cpu "-DAOM_TARGET_CPU=generic")
endif()

if(VCPKG_TARGET_ARCHITECTURE STREQUAL "arm" AND VCPKG_TARGET_IS_LINUX)
  set(aom_target_cpu "-DENABLE_NEON=OFF")
endif()

vcpkg_cmake_configure(
    SOURCE_PATH ${SOURCE_PATH}
    OPTIONS
        ${aom_target_cpu}
        -DENABLE_DOCS=OFF
        -DENABLE_APPS=OFF
        -DENABLE_EXAMPLES=OFF
        -DENABLE_TESTDATA=OFF
        -DENABLE_TESTS=OFF
        -DENABLE_TOOLS=OFF
        -DTHREADS_PREFER_PTHREAD_FLAG=ON
        ${aom_nasm_compiler}
        "-DPERL_EXECUTABLE=${PERL}"
)

vcpkg_cmake_install()

vcpkg_copy_pdbs()

if(DEFINED ENV{USE_AOM_391})
    vcpkg_cmake_config_fixup(CONFIG_PATH lib/cmake/aom)
    if(VCPKG_TARGET_IS_WINDOWS)
        vcpkg_replace_string("${CURRENT_PACKAGES_DIR}/lib/pkgconfig/aom.pc" " -lm" "")
        if(NOT VCPKG_BUILD_TYPE)
            vcpkg_replace_string("${CURRENT_PACKAGES_DIR}/debug/lib/pkgconfig/aom.pc" " -lm" "")
        endif()
    endif()
else()
    vcpkg_cmake_config_fixup(CONFIG_PATH lib/cmake/AOM)
endif()

vcpkg_fixup_pkgconfig()

file(REMOVE_RECURSE
    "${CURRENT_PACKAGES_DIR}/debug/include"
    "${CURRENT_PACKAGES_DIR}/debug/share"
)

if(DEFINED ENV{USE_AOM_391})
    vcpkg_install_copyright(FILE_LIST "${SOURCE_PATH}/LICENSE" "${SOURCE_PATH}/PATENTS")
else()
    vcpkg_install_copyright(FILE_LIST
        "${SOURCE_PATH}/LICENSE"
        "${SOURCE_PATH}/PATENTS"
        "${SOURCE_PATH}/third_party/fastfeat/LICENSE"
        "${SOURCE_PATH}/third_party/vector/LICENSE"
        "${SOURCE_PATH}/third_party/x86inc/LICENSE"
    )
endif()
