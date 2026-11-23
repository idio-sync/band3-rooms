"""python -m unittest discover -s site/tools (from the repo root)"""

import unittest

from make_songs_json import parse, songs

DLC = r'''
; a DLC package's songs.dta, as Harmonix wrote them
(
   'mysong'
   (
      'name'
      "My \q Song\q"
   )
   ('artist' "Some Band")
   ('master' 1)
   ('song_id' 1234567)
   ('song' ('name' "songs/mysong/mysong") ('tracks' (('drum' (0 1)))))
   ('encoding' 'utf8')
)
(other (name "Custom") (artist "Me") (song_id othercustom))
'''

DISC = '''
#ifndef COMMON_RG_SONG_DATA
#define COMMON_RG_SONG_DATA
((name "Lessons") (artist "Counting Bros") (fake TRUE))
#endif
#ifndef NODISK_SONGS
(real (name "Real Song") (artist "Real Artist") (song_id 1001))
#endif
#ifndef _SHIP
(debug (name "Real Song NO BASS") (artist "Real Artist") (song_id 1145))
#endif
(lesson (song_id 1201) #merge COMMON_RG_SONG_DATA)
(fakesong (name "Fake") (artist "Fake") (song_id 9) (fake TRUE))
'''


class Test(unittest.TestCase):
    def test_dlc(self):
        self.assertEqual(songs(parse(DLC)), {1234567: {'name': 'My " Song"', 'artist': 'Some Band'}})

    def test_disc(self):
        self.assertEqual(songs(parse(DISC)), {1001: {'name': 'Real Song', 'artist': 'Real Artist'}})


if __name__ == '__main__':
    unittest.main()
